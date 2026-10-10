package ocr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// call records one fake command invocation.
type call struct {
	bin   string
	args  []string
	stdin []byte
}

// fakeRunner records invocations and delegates to fn (when set).
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	fn    func(ctx context.Context, stdin []byte, bin string, args []string) ([]byte, error)
}

func (f *fakeRunner) Run(ctx context.Context, stdin []byte, bin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{
		bin:   bin,
		args:  append([]string(nil), args...),
		stdin: append([]byte(nil), stdin...),
	})
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, stdin, bin, args)
	}
	return nil, nil
}

func (f *fakeRunner) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func TestDockerTesseractOCRSuccess(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []byte, string, []string) ([]byte, error) {
		return []byte("  hello world\n"), nil
	}}
	d := NewDockerTesseract(Config{Image: "myogchat-ocr", Langs: "eng", Docker: "/usr/bin/docker", Runner: fr})

	got, err := d.OCR(context.Background(), []byte("PNGBYTES"))
	if err != nil {
		t.Fatalf("OCR: %v", err)
	}
	if got != "hello world" {
		t.Errorf("OCR = %q, want %q", got, "hello world")
	}

	calls := fr.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.bin != "/usr/bin/docker" {
		t.Errorf("bin = %q, want /usr/bin/docker", c.bin)
	}
	if string(c.stdin) != "PNGBYTES" {
		t.Errorf("stdin = %q, want image bytes", c.stdin)
	}
	joined := strings.Join(c.args, " ")
	for _, want := range []string{
		"run --rm",
		"--name myogchat-ocr-",
		"--network none",
		"--memory 256m",
		"--cpus 1",
		"--pids-limit 64",
		"--read-only",
		"--tmpfs /tmp:rw,size=32m",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--user 65534:65534",
		"-i myogchat-ocr",
		"tesseract stdin stdout -l eng",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
}

func TestDockerTesseractOCRUsesConfiguredLangs(t *testing.T) {
	fr := &fakeRunner{}
	d := NewDockerTesseract(Config{Image: "img", Langs: "eng+deu", Runner: fr})
	if _, err := d.OCR(context.Background(), []byte("x")); err != nil {
		t.Fatalf("OCR: %v", err)
	}
	joined := strings.Join(fr.snapshot()[0].args, " ")
	if !strings.Contains(joined, "tesseract stdin stdout -l eng+deu") {
		t.Errorf("argv %q missing configured langs", joined)
	}
}

func TestDockerTesseractOCRTimeoutCleansUp(t *testing.T) {
	fr := &fakeRunner{fn: func(ctx context.Context, _ []byte, _ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rm" {
			return nil, nil // cleanup succeeds immediately
		}
		<-ctx.Done() // simulate a hung container until the deadline fires
		return nil, ctx.Err()
	}}
	d := NewDockerTesseract(Config{Timeout: 20 * time.Millisecond, Runner: fr})

	_, err := d.OCR(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout message", err)
	}

	calls := fr.snapshot()
	var runName string
	var rm *call
	for i := range calls {
		args := calls[i].args
		if len(args) == 0 {
			continue
		}
		switch args[0] {
		case "run":
			for j := 0; j+1 < len(args); j++ {
				if args[j] == "--name" {
					runName = args[j+1]
				}
			}
		case "rm":
			rm = &calls[i]
		}
	}
	if runName == "" {
		t.Fatal("no docker run call recorded")
	}
	if rm == nil {
		t.Fatal("expected a docker rm -f cleanup call after a timeout")
	}
	if len(rm.args) != 3 || rm.args[1] != "-f" || rm.args[2] != runName {
		t.Errorf("cleanup args = %q, want [rm -f %s]", rm.args, runName)
	}
}

func TestDockerTesseractOCRFailureNoCleanup(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []byte, string, []string) ([]byte, error) {
		return nil, errors.New("docker: image not found")
	}}
	d := NewDockerTesseract(Config{Runner: fr})

	_, err := d.OCR(context.Background(), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("err = %v, want the docker failure", err)
	}
	if n := len(fr.snapshot()); n != 1 {
		t.Errorf("got %d calls, want 1 (no cleanup for a fast failure)", n)
	}
}

func TestDockerTesseractOCREmptyImage(t *testing.T) {
	d := NewDockerTesseract(Config{Runner: &fakeRunner{}})
	if _, err := d.OCR(context.Background(), nil); err == nil {
		t.Fatal("expected an error for an empty image")
	}
}

func TestDockerTesseractDefaults(t *testing.T) {
	d := NewDockerTesseract(Config{})
	if d.image != DefaultImage || d.langs != DefaultLangs || d.docker != DefaultDocker {
		t.Errorf("defaults = %q/%q/%q", d.image, d.langs, d.docker)
	}
	if d.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", d.timeout, DefaultTimeout)
	}
	if _, ok := d.runner.(ExecRunner); !ok {
		t.Errorf("runner = %T, want ExecRunner", d.runner)
	}
}

func TestFuncAdapter(t *testing.T) {
	fr := &fakeRunner{fn: func(context.Context, []byte, string, []string) ([]byte, error) {
		return []byte("adapted"), nil
	}}
	d := NewDockerTesseract(Config{Runner: fr})
	fn := Func(context.Background(), d)
	got, err := fn([]byte("x"))
	if err != nil || got != "adapted" {
		t.Fatalf("Func() = %q, %v; want adapted, nil", got, err)
	}
}

// TestLiveSmokeDockerTesseract exercises a real docker build + OCR run. It is
// opt-in (MYOGCHAT_OCR_LIVE=1) and skips when docker, the image, or the image
// rendering tool is unavailable, so it never breaks `go test ./...`.
func TestLiveSmokeDockerTesseract(t *testing.T) {
	if os.Getenv("MYOGCHAT_OCR_LIVE") != "1" {
		t.Skip("set MYOGCHAT_OCR_LIVE=1 to run the docker OCR smoke test")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}
	if err := exec.Command(docker, "info").Run(); err != nil {
		t.Skipf("docker daemon unavailable: %v", err)
	}
	image := os.Getenv("MYOGCHAT_OCR_IMAGE")
	if image == "" {
		image = DefaultImage
	}
	if err := exec.Command(docker, "image", "inspect", image).Run(); err != nil {
		t.Skipf("ocr image %q not built (run deploy/ocr-setup.sh)", image)
	}
	convert, err := exec.LookPath("convert")
	if err != nil {
		convert, err = exec.LookPath("magick")
	}
	if err != nil {
		t.Skip("ImageMagick not found to render a test PNG")
	}

	dir := t.TempDir()
	png := filepath.Join(dir, "hello.png")
	render := exec.Command(convert, "-size", "420x140", "xc:white",
		"-gravity", "center", "-pointsize", "80", "-fill", "black",
		"-annotate", "0", "hello", png)
	if out, err := render.CombinedOutput(); err != nil {
		t.Skipf("render test PNG: %v: %s", err, out)
	}
	data, err := os.ReadFile(png)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	d := NewDockerTesseract(Config{Image: image, Docker: docker})
	got, err := d.OCR(context.Background(), data)
	if err != nil {
		t.Fatalf("OCR: %v", err)
	}
	if !strings.Contains(strings.ToLower(got), "hello") {
		t.Errorf("OCR = %q, want it to contain %q", got, "hello")
	}
}
