package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// liveRunner returns a runner for the real Docker daemon, or skips the test
// when Docker is not available (no binary / daemon / permission).
func liveRunner(t *testing.T) (*Runner, context.Context) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live Docker sandbox test in -short mode")
	}
	cfg := DefaultConfig()
	cfg.Timeout = 60 * time.Second
	r := NewRunner(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if !r.Available(ctx) {
		t.Skip("docker daemon not reachable; skipping live sandbox test")
	}
	return r, ctx
}

// skipIfUnprovisioned converts provisioning failures (image/network missing)
// into a skip, so the test only runs where deploy/sandbox-setup.sh has been run.
func skipIfUnprovisioned(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, ErrImageMissing) {
		t.Skip("sandbox images not pulled; run deploy/sandbox-setup.sh")
	}
	if errors.Is(err, ErrNetworkMissing) {
		t.Skip("sandbox network unavailable; run deploy/sandbox-setup.sh")
	}
}

func TestLiveDockerRunBash(t *testing.T) {
	r, ctx := liveRunner(t)
	res, err := r.Run(ctx, Spec{Language: LanguageBash, Code: "echo hello; echo oops >&2; exit 3"})
	skipIfUnprovisioned(t, err)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Errorf("stderr = %q", res.Stderr)
	}
	if res.TimedOut {
		t.Error("TimedOut = true")
	}
}

func TestLiveDockerRunPython(t *testing.T) {
	r, ctx := liveRunner(t)
	res, err := r.Run(ctx, Spec{Language: LanguagePython, Code: "print(6 * 7)"})
	skipIfUnprovisioned(t, err)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "42") {
		t.Errorf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestLiveEphemeralFilesystem proves state does not persist between calls.
func TestLiveEphemeralFilesystem(t *testing.T) {
	r, ctx := liveRunner(t)
	write, err := r.Run(ctx, Spec{Language: LanguageBash, Code: "echo hi > /work/state.txt; cat /work/state.txt"})
	skipIfUnprovisioned(t, err)
	if err != nil {
		t.Fatalf("write run: %v", err)
	}
	if !strings.Contains(write.Stdout, "hi") {
		t.Fatalf("write stdout = %q", write.Stdout)
	}
	read, err := r.Run(ctx, Spec{Language: LanguageBash, Code: "ls /work/state.txt 2>&1 || echo GONE"})
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if !strings.Contains(read.Stdout, "GONE") {
		t.Errorf("state persisted across calls: %q", read.Stdout)
	}
}

// TestLiveReadOnlyRootfs proves the root filesystem is read-only.
func TestLiveReadOnlyRootfs(t *testing.T) {
	r, ctx := liveRunner(t)
	res, err := r.Run(ctx, Spec{Language: LanguageBash, Code: "touch /root-should-fail 2>&1 || echo RO"})
	skipIfUnprovisioned(t, err)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "RO") {
		t.Errorf("root filesystem appears writable: %q", res.Stdout)
	}
}
