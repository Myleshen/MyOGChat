// Package sandbox runs model-generated code inside a hardened, resource-limited,
// ephemeral Docker container (Phase 2.6 Python / 2.7 Bash).
//
// Each call starts a brand-new container (`docker run --rm`) with a read-only
// root filesystem, a non-root user, all capabilities dropped, a dedicated
// user-defined bridge network (no published ports → no inbound path), strict
// CPU/memory/PID limits and a wall-clock timeout. The code is fed to the
// interpreter over stdin; it is NEVER interpolated into the command line.
//
// The package is deliberately free of server/upstream types so it can be unit
// tested with an injected command executor (no Docker required).
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Language identifiers accepted by Spec.
const (
	LanguageBash   = "bash"
	LanguagePython = "python"
)

// Defaults (all overridable via environment — see ConfigFromEnv).
const (
	DefaultDocker      = "/usr/bin/docker"
	DefaultNetwork     = "myogchat-sbx"
	DefaultBashImage   = "bash:5.3"
	DefaultPythonImage = "myogchat-sandbox-python"
	DefaultMemory      = "256m"
	DefaultCPUs        = "1"
	DefaultPidsLimit   = 128
	DefaultNoFile      = "256:256"
	DefaultTmpSize     = "16m"
	DefaultMaxOutput   = 100_000
	DefaultTimeout     = 30 * time.Second

	// Artifact harvest caps (5.8). A per-file docker --ulimit fsize gives the
	// hard in-container cap; the harvest caps are the second line of defence
	// against storing too much.
	DefaultMaxFileSize   = 10 << 20 // 10 MB
	DefaultMaxTotalSize  = 25 << 20 // 25 MB
	DefaultMaxFiles      = 20
	DefaultFileSizeLimit = "10485760:10485760" // docker --ulimit fsize soft:hard
	DefaultWorkDirName   = "myogchat/sandbox"  // under ~/.local/share

	// workspaceMaxAge bounds how long a crashed run's workspace may survive
	// before the startup/hourly sweep removes it. Real runs are <= Timeout.
	workspaceMaxAge = time.Hour

	// LabelKey/Value tag every sandbox container so leaked containers can be
	// cleaned up with `docker rm -f $(docker ps -aq --filter label=...)`.
	LabelKey   = "myogchat-sandbox"
	LabelValue = "1"

	// containerUser is nobody:nogroup — the container never runs as root.
	containerUser = "65534:65534"

	// probeTTL bounds how often the Docker reachability probe runs.
	probeTTL = 30 * time.Second

	// auxiliaryTimeout bounds the short helper commands (network create,
	// probe, cleanup) so they cannot hang the request.
	auxiliaryTimeout = 10 * time.Second

	// truncMarker is appended when captured output is clipped.
	truncMarker = "\n…[output truncated]"
)

// Sentinel errors, returned by Run and matched with errors.Is. Container
// scripts that exit non-zero are NOT errors — they come back as a Result.
var (
	// ErrDockerNotFound means the docker executable could not be located.
	ErrDockerNotFound = errors.New("docker executable not found (set MYOGCHAT_DOCKER)")
	// ErrDockerUnavailable means the docker daemon is not reachable.
	ErrDockerUnavailable = errors.New("docker daemon is not reachable")
	// ErrImageMissing means the sandbox image is not present locally.
	ErrImageMissing = errors.New("sandbox image is not available locally")
	// ErrNetworkMissing means the sandbox network could not be created.
	ErrNetworkMissing = errors.New("sandbox docker network is unavailable")
	// ErrTimeout means the container exceeded the per-call wall-clock limit and
	// was force-removed.
	ErrTimeout = errors.New("sandbox execution timed out")
)

// Config holds the hardened container parameters.
type Config struct {
	Docker      string        // path to the docker CLI
	Network     string        // dedicated user-defined bridge network
	BashImage   string        // image for run_bash
	PythonImage string        // image for run_python
	Timeout     time.Duration // per-call wall-clock limit
	Memory      string        // docker --memory (and --memory-swap) value
	CPUs        string        // docker --cpus value
	PidsLimit   int           // docker --pids-limit value
	NoFile      string        // docker --ulimit nofile= value (soft:hard)
	TmpSize     string        // size of the /tmp tmpfs
	MaxOutput   int           // max characters of stdout+stderr returned
	// Workspace + artifact harvest (5.8).
	WorkDir       string // host directory root for per-run workspaces (never /tmp)
	FileSizeLimit string // docker --ulimit fsize= value (soft:hard)
	MaxFileSize   int64  // per-file harvest cap (bytes)
	MaxTotalSize  int64  // total harvest cap (bytes)
	MaxFiles      int    // max harvested files per run
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Docker:      resolveDocker(""),
		Network:     DefaultNetwork,
		BashImage:   DefaultBashImage,
		PythonImage: DefaultPythonImage,
		Timeout:     DefaultTimeout,
		Memory:      DefaultMemory,
		CPUs:        DefaultCPUs,
		PidsLimit:   DefaultPidsLimit,
		NoFile:      DefaultNoFile,
		TmpSize:     DefaultTmpSize,
		MaxOutput:   DefaultMaxOutput,

		WorkDir:       resolveWorkDir(""),
		FileSizeLimit: DefaultFileSizeLimit,
		MaxFileSize:   DefaultMaxFileSize,
		MaxTotalSize:  DefaultMaxTotalSize,
		MaxFiles:      DefaultMaxFiles,
	}
}

// ConfigFromEnv overlays environment variables on DefaultConfig:
//
//	MYOGCHAT_DOCKER               docker CLI path (default /usr/bin/docker, else LookPath)
//	MYOGCHAT_SANDBOX_NETWORK      network name (default myogchat-sbx)
//	MYOGCHAT_SANDBOX_IMAGE_BASH   bash image (default bash:5.3)
//	MYOGCHAT_SANDBOX_IMAGE_PYTHON python image (default myogchat-sandbox-python)
//	MYOGCHAT_SANDBOX_MEMORY       memory limit (default 256m)
//	MYOGCHAT_SANDBOX_CPUS         CPU limit (default 1)
//	MYOGCHAT_SANDBOX_TIMEOUT      seconds (default 30)
//	MYOGCHAT_SANDBOX_MAX_OUTPUT   characters (default 100000)
//	MYOGCHAT_SANDBOX_WORKDIR      host root for per-run workspaces (default
//	                              ~/.local/share/myogchat/sandbox)
func ConfigFromEnv() Config {
	c := DefaultConfig()
	c.Docker = resolveDocker(os.Getenv("MYOGCHAT_DOCKER"))
	if v := os.Getenv("MYOGCHAT_SANDBOX_NETWORK"); v != "" {
		c.Network = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_IMAGE_BASH"); v != "" {
		c.BashImage = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_IMAGE_PYTHON"); v != "" {
		c.PythonImage = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_MEMORY"); v != "" {
		c.Memory = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_CPUS"); v != "" {
		c.CPUs = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Timeout = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_MAX_OUTPUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxOutput = n
		}
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_WORKDIR"); v != "" {
		c.WorkDir = v
	}
	return c
}

// EnabledFromEnv reports whether the sandbox is enabled at all via
// MYOGCHAT_SANDBOX (a hard kill switch, default on). Accepted false values:
// 0, false, off, no (case-insensitive).
func EnabledFromEnv() bool {
	v := strings.TrimSpace(os.Getenv("MYOGCHAT_SANDBOX"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// resolveDocker picks the docker CLI: an explicit value wins; otherwise
// /usr/bin/docker when present, falling back to $PATH.
func resolveDocker(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat(DefaultDocker); err == nil {
		return DefaultDocker
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	return DefaultDocker
}

// resolveWorkDir picks the host root for per-run sandbox workspaces. An explicit
// value wins; otherwise ~/.local/share/myogchat/sandbox. /tmp is deliberately
// avoided: the systemd unit sets PrivateTmp=true, so a container in a separate
// mount namespace could not see a host /tmp path anyway.
func resolveWorkDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Degenerate fallback (no home dir); the per-run dir is still created
		// with a random name and removed on completion.
		return filepath.Join(os.TempDir(), "myogchat-sandbox")
	}
	return filepath.Join(home, ".local", "share", filepath.FromSlash(DefaultWorkDirName))
}

// Spec is one execution request.
type Spec struct {
	Language string // LanguageBash or LanguagePython
	Code     string // fed to the interpreter on stdin
}

// Result is the outcome of a sandboxed run.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
	// Artifacts are the regular files the script left in /work, harvested after
	// the run (name sanitized, MIME sniffed, caps enforced). Empty when none.
	Artifacts []Artifact
}

// Artifact is one file produced by a sandbox run: a sanitized base name, a
// sniffed MIME type and the raw bytes. The bytes are stored opaque and served
// only as a download (5.8).
type Artifact struct {
	Name string
	MIME string
	Data []byte
}

// ExecFunc runs one command and returns captured stdout/stderr. It is injectable
// so the runner is unit-testable without Docker.
type ExecFunc func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr []byte, err error)

// Runner executes code in ephemeral sandbox containers.
type Runner struct {
	cfg  Config
	exec ExecFunc

	mu       sync.Mutex
	probeOK  bool
	probeAt  time.Time
	netReady bool
}

// NewRunner returns a Runner using the real os/exec executor.
func NewRunner(cfg Config) *Runner {
	return &Runner{cfg: cfg, exec: osExec}
}

// NewRunnerWithExec returns a Runner using a custom executor (tests).
func NewRunnerWithExec(cfg Config, exec ExecFunc) *Runner {
	return &Runner{cfg: cfg, exec: exec}
}

// Config returns the runner's configuration.
func (r *Runner) Config() Config { return r.cfg }

// Docker returns the docker CLI path in use (for status display).
func (r *Runner) Docker() string { return r.cfg.Docker }

// osExec is the default ExecFunc: exec.CommandContext with piped stdin/stdout.
func osExec(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// Available reports whether the Docker daemon is reachable. The result is cached
// for probeTTL so per-request registration does not shell out every time.
func (r *Runner) Available(ctx context.Context) bool {
	r.mu.Lock()
	if !r.probeAt.IsZero() && time.Since(r.probeAt) < probeTTL {
		ok := r.probeOK
		r.mu.Unlock()
		return ok
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
	defer cancel()
	_, _, err := r.exec(ctx, r.cfg.Docker, []string{"info", "--format", "{{.ServerVersion}}"}, nil)
	ok := err == nil

	r.mu.Lock()
	r.probeOK = ok
	r.probeAt = time.Now()
	r.mu.Unlock()
	return ok
}

// Run executes spec and returns its result. A non-zero exit code is reported in
// Result (not as an error); failures to launch the container return a sentinel
// error (ErrImageMissing, ErrNetworkMissing, ErrDockerUnavailable, …).
func (r *Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	image, command, err := r.imageAndCommand(spec)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(spec.Code) == "" {
		return Result{}, errors.New("code is required")
	}
	if err := r.ensureNetwork(ctx); err != nil {
		return Result{}, err
	}

	name := r.containerName()
	runDir, cleanupDir, err := r.prepareWorkspace(name)
	if err != nil {
		return Result{}, err
	}
	defer cleanupDir()
	args := r.runArgs(image, command, name, runDir)

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	start := time.Now()
	stdout, stderr, execErr := r.exec(runCtx, r.cfg.Docker, args, []byte(spec.Code))
	res := Result{
		Stdout:   string(stdout),
		Stderr:   string(stderr),
		Duration: time.Since(start),
	}
	// Harvest the workspace before the container is force-removed and before the
	// deferred RemoveAll. The harvest never follows symlinks and enforces the
	// per-file/count/total caps (5.8).
	res.Artifacts = harvest(runDir, r.cfg)
	res.Truncated = r.truncate(&res)

	// Timeout: the container must not leak, so force-remove it by name.
	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		r.cleanup(name)
		return res, fmt.Errorf("%w after %s", ErrTimeout, r.cfg.Timeout)
	}
	// Caller cancelled (e.g. client disconnect): also remove the container.
	if ctx.Err() != nil {
		r.cleanup(name)
		return res, ctx.Err()
	}
	if execErr != nil {
		code := exitCode(execErr)
		// Exit code 125 is docker run's own failure; known daemon/image/network
		// messages are docker-level regardless of the code.
		if code == 125 || dockerLevelFailure(res.Stderr) || code < 0 {
			return res, classifyRunError(execErr, res.Stderr)
		}
		// The container ran and its command exited non-zero — a normal result.
		res.ExitCode = code
		return res, nil
	}
	res.ExitCode = 0
	return res, nil
}

// imageAndCommand maps a Spec to its image and container command. The command
// reads the program from stdin (`bash -s` / `python -`); the code itself is
// never placed on the command line.
func (r *Runner) imageAndCommand(spec Spec) (string, []string, error) {
	switch spec.Language {
	case LanguageBash:
		return r.cfg.BashImage, []string{"bash", "-s"}, nil
	case LanguagePython:
		return r.cfg.PythonImage, []string{"python", "-"}, nil
	default:
		return "", nil, fmt.Errorf("unknown sandbox language %q", spec.Language)
	}
}

// prepareWorkspace creates a fresh per-run host directory that is bind-mounted
// rw into the container as /work. The parent root is 0700 (only the service
// user can traverse it); the per-run dir is made group/other-writable so the
// unprivileged container user 65534 can write into it. The returned cleanup
// removes the dir; callers defer it.
func (r *Runner) prepareWorkspace(name string) (string, func(), error) {
	root := r.cfg.WorkDir
	if root == "" {
		return "", func() {}, errors.New("sandbox workdir is not configured")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create sandbox workdir: %w", err)
	}
	runDir := filepath.Join(root, name)
	if err := os.Mkdir(runDir, 0o777); err != nil {
		return "", func() {}, fmt.Errorf("create sandbox workspace: %w", err)
	}
	// Mkdir is filtered by umask, so make the mode explicit: the container user
	// (65534) is no relation of the service user and must be able to write.
	if err := os.Chmod(runDir, 0o777); err != nil {
		_ = os.RemoveAll(runDir)
		return "", func() {}, fmt.Errorf("chmod sandbox workspace: %w", err)
	}
	return runDir, func() { _ = os.RemoveAll(runDir) }, nil
}

// SweepLeftovers removes per-run workspaces older than workspaceMaxAge. It
// covers crash leftovers (a SIGKILL mid-run skips the deferred RemoveAll) and is
// safe to call at startup and hourly from the retention janitor.
func (r *Runner) SweepLeftovers() {
	root := r.cfg.WorkDir
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-workspaceMaxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// runArgs builds the full hardened `docker run` argv.
func (r *Runner) runArgs(image string, command []string, name, workDir string) []string {
	args := []string{
		"run",
		"--rm",
		"--name", name,
		"--label", LabelKey + "=" + LabelValue,
		"--network", r.cfg.Network,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", containerUser,
		"--read-only",
		// /work is a host bind mount so produced files survive the container and
		// can be harvested; /tmp stays an ephemeral tmpfs.
		"-v", workDir + ":/work:rw",
		"--tmpfs", "/tmp:rw,size=" + r.cfg.TmpSize,
		"--workdir", "/work",
		"--memory", r.cfg.Memory,
		"--memory-swap", r.cfg.Memory,
		"--cpus", r.cfg.CPUs,
		"--pids-limit", strconv.Itoa(r.cfg.PidsLimit),
		"--ulimit", "nofile=" + r.cfg.NoFile,
		// A hard per-file size cap so one runaway write cannot exhaust the disk.
		"--ulimit", "fsize=" + r.cfg.FileSizeLimit,
		"--stop-timeout", "2",
		"--pull", "never",
		"-e", "HOME=/work",
		"-e", "TMPDIR=/tmp",
		"-i",
		image,
	}
	return append(args, command...)
}

// ensureNetwork makes sure the dedicated bridge network exists, creating it on
// first use. The result is cached for the runner's lifetime.
func (r *Runner) ensureNetwork(ctx context.Context) error {
	r.mu.Lock()
	if r.netReady {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	inspectCtx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
	_, _, err := r.exec(inspectCtx, r.cfg.Docker, []string{"network", "inspect", r.cfg.Network}, nil)
	cancel()
	if err == nil {
		r.mu.Lock()
		r.netReady = true
		r.mu.Unlock()
		return nil
	}

	createCtx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
	_, stderr, createErr := r.exec(createCtx, r.cfg.Docker, []string{"network", "create", r.cfg.Network}, nil)
	cancel()
	if createErr != nil {
		// A concurrent creator may have won the race; re-check before failing.
		reCtx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
		_, _, reErr := r.exec(reCtx, r.cfg.Docker, []string{"network", "inspect", r.cfg.Network}, nil)
		cancel()
		if reErr != nil {
			return fmt.Errorf("%w: %s", ErrNetworkMissing, firstLine(string(stderr)))
		}
	}
	r.mu.Lock()
	r.netReady = true
	r.mu.Unlock()
	return nil
}

// cleanup force-removes a container by name so a killed `docker run` client
// cannot leak it. It uses a fresh context: the caller's is already cancelled.
func (r *Runner) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), auxiliaryTimeout)
	defer cancel()
	_, _, _ = r.exec(ctx, r.cfg.Docker, []string{"rm", "-f", name}, nil)
}

// truncate clips stdout+stderr to MaxOutput characters, stdout first. It
// reports whether anything was dropped.
func (r *Runner) truncate(res *Result) bool {
	limit := r.cfg.MaxOutput
	if limit <= 0 {
		return false
	}
	stdout := []rune(res.Stdout)
	if len(stdout) >= limit {
		res.Stdout = string(stdout[:limit]) + truncMarker
		res.Stderr = ""
		return true
	}
	stderr := []rune(res.Stderr)
	if remaining := limit - len(stdout); len(stderr) > remaining {
		res.Stderr = string(stderr[:remaining]) + truncMarker
		return true
	}
	return false
}

// containerName returns a unique name for one run.
func (r *Runner) containerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("myogchat-sbx-%d", time.Now().UnixNano())
	}
	return "myogchat-sbx-" + hex.EncodeToString(b[:])
}

// exitCode extracts a process exit code from an error, or -1.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// dockerLevelFailure reports whether stderr carries a Docker-level (not script)
// failure message. Markers are specific so an ordinary script's stderr cannot
// be misclassified.
func dockerLevelFailure(stderr string) bool {
	low := strings.ToLower(stderr)
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"permission denied while trying to connect",
		"unable to find image",
		"no such image",
		"pull access denied",
		"manifest unknown",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return strings.Contains(low, "network") && strings.Contains(low, "not found")
}

// classifyRunError maps a Docker launch failure to a clear, actionable error.
func classifyRunError(execErr error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "cannot connect to the docker daemon"),
		strings.Contains(low, "permission denied while trying to connect"):
		return fmt.Errorf("%w: %s", ErrDockerUnavailable, firstLine(stderr))
	case strings.Contains(low, "unable to find image"),
		strings.Contains(low, "no such image"),
		strings.Contains(low, "pull access denied"),
		strings.Contains(low, "manifest unknown"):
		return fmt.Errorf("%w: run deploy/sandbox-setup.sh to pull the images (%s)", ErrImageMissing, firstLine(stderr))
	case strings.Contains(low, "network") && strings.Contains(low, "not found"):
		return fmt.Errorf("%w: run deploy/sandbox-setup.sh to create it", ErrNetworkMissing)
	}
	if errors.Is(execErr, exec.ErrNotFound) || errors.Is(execErr, fs.ErrNotExist) {
		return ErrDockerNotFound
	}
	return fmt.Errorf("docker run failed: %w", execErr)
}

// firstLine returns the first non-empty line of s, trimmed and capped.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
