// Package ocr turns image bytes into text for the attachment pipeline
// (feature 2.5).
//
// OCR runs Tesseract inside an ephemeral, hardened Docker container: the host
// needs no tesseract install (the Pi has none and sudo needs a password) and
// the container's resource limits protect the Pi's RAM/CPU. The image is piped
// in via stdin and the recognized text comes back on stdout, so nothing is
// written to a shared filesystem.
package ocr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Defaults for the Docker Tesseract engine.
const (
	// DefaultImage is the docker tag built by deploy/ocr-setup.sh.
	DefaultImage = "myogchat-ocr"
	// DefaultLangs is the Tesseract language list (see MYOGCHAT_OCR_LANGS).
	DefaultLangs = "eng"
	// DefaultDocker is the docker binary path on the Pi.
	DefaultDocker = "/usr/bin/docker"
	// DefaultTimeout bounds a single OCR run.
	DefaultTimeout = 30 * time.Second
	// removeTimeout bounds the best-effort `docker rm -f` cleanup.
	removeTimeout = 10 * time.Second
)

// OCRFunc recognizes text in image bytes. It is the seam internal/extract uses
// so the extractor (and its tests) never depend on Docker.
type OCRFunc func(image []byte) (string, error)

// Engine recognizes text in an image.
type Engine interface {
	// OCR returns the text found in image. ctx bounds the work.
	OCR(ctx context.Context, image []byte) (string, error)
}

// Func adapts an Engine to an OCRFunc bound to ctx.
func Func(ctx context.Context, e Engine) OCRFunc {
	return func(image []byte) (string, error) { return e.OCR(ctx, image) }
}

// CommandRunner runs one external command with optional stdin, returning its
// stdout. It is injectable so tests can exercise the docker invocation without
// a daemon.
type CommandRunner interface {
	Run(ctx context.Context, stdin []byte, bin string, args ...string) ([]byte, error)
}

// ExecRunner is the production CommandRunner backed by os/exec.
type ExecRunner struct{}

// Run executes bin with args, feeding stdin and capturing stdout. stderr is
// folded into the returned error so failures are diagnosable.
func (ExecRunner) Run(ctx context.Context, stdin []byte, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s: %w", bin, err)
		}
		return nil, fmt.Errorf("%s: %w: %s", bin, err, msg)
	}
	return stdout.Bytes(), nil
}

// Config configures a DockerTesseract engine. Empty fields take their default.
type Config struct {
	// Image is the docker image tag containing tesseract.
	Image string
	// Langs is the tesseract language list, e.g. "eng" or "eng+deu".
	Langs string
	// Docker is the docker binary path.
	Docker string
	// Timeout bounds one OCR run.
	Timeout time.Duration
	// Runner overrides command execution (tests); nil uses ExecRunner.
	Runner CommandRunner
}

// DockerTesseract runs `tesseract` in an ephemeral, hardened container, reading
// the image from stdin and writing text to stdout.
type DockerTesseract struct {
	image   string
	langs   string
	docker  string
	timeout time.Duration
	runner  CommandRunner
}

// NewDockerTesseract builds an engine, applying defaults for empty fields.
func NewDockerTesseract(cfg Config) *DockerTesseract {
	d := &DockerTesseract{
		image:   cfg.Image,
		langs:   cfg.Langs,
		docker:  cfg.Docker,
		timeout: cfg.Timeout,
		runner:  cfg.Runner,
	}
	if d.image == "" {
		d.image = DefaultImage
	}
	if d.langs == "" {
		d.langs = DefaultLangs
	}
	if d.docker == "" {
		d.docker = DefaultDocker
	}
	if d.timeout <= 0 {
		d.timeout = DefaultTimeout
	}
	if d.runner == nil {
		d.runner = ExecRunner{}
	}
	return d
}

// containerName returns a unique, predictable container name so a timed-out run
// can be force-removed by name.
func containerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand is effectively infallible; fall back to a time-based name
		// rather than panicking in a request path.
		return fmt.Sprintf("myogchat-ocr-%d", time.Now().UnixNano())
	}
	return "myogchat-ocr-" + hex.EncodeToString(b[:])
}

// runArgs is the hardened `docker run` argv (without the leading docker binary).
// It mirrors the sandbox hardening used elsewhere: no network, capped
// memory/cpu/pids, read-only rootfs with a small tmpfs, no capabilities, no
// privilege escalation, and a non-root user.
func (d *DockerTesseract) runArgs(name string) []string {
	return []string{
		"run", "--rm",
		"--name", name,
		"--network", "none",
		"--memory", "256m",
		"--cpus", "1",
		"--pids-limit", "64",
		"--read-only",
		"--tmpfs", "/tmp:rw,size=32m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"-i",
		d.image,
		"tesseract", "stdin", "stdout",
		"-l", d.langs,
	}
}

// OCR recognizes text in image, returning it with surrounding whitespace
// trimmed. A non-empty image is required; a timeout force-removes the container
// because `--rm` alone cannot clean up a still-running process.
func (d *DockerTesseract) OCR(ctx context.Context, image []byte) (string, error) {
	if len(image) == 0 {
		return "", errors.New("ocr: empty image")
	}
	name := containerName()
	runCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	out, err := d.runner.Run(runCtx, image, d.docker, d.runArgs(name)...)
	if err != nil {
		// The deadline (or a caller cancellation) left the container running;
		// remove it explicitly so it does not linger.
		if runCtx.Err() != nil {
			d.forceRemove(name)
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("ocr: timed out after %s", d.timeout)
			}
			return "", fmt.Errorf("ocr: %w", runCtx.Err())
		}
		return "", fmt.Errorf("ocr: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// forceRemove best-effort removes a container left behind by a timeout, using a
// fresh context so cleanup is not itself cancelled.
func (d *DockerTesseract) forceRemove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	_, _ = d.runner.Run(ctx, nil, d.docker, "rm", "-f", name)
}
