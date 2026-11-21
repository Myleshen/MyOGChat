package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ulimitValue returns the value of `--ulimit <key>=…` in docker run args.
func ulimitValue(args []string, key string) string {
	for i, a := range args {
		if a == "--ulimit" && i+1 < len(args) {
			if v, ok := strings.CutPrefix(args[i+1], key+"="); ok {
				return v
			}
		}
	}
	return ""
}

// writeViaRun is a fake executor that writes one file into the bind-mounted
// workspace on every `docker run`.
func writeViaRun(name, data string) func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "run" {
			dir := strings.TrimSuffix(argValue(args, "-v"), ":/work:rw")
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
				return nil, nil, err
			}
		}
		return []byte("ok"), nil, nil
	}
}

// TestPersistentWorkspaceSurvivesRuns checks that a Spec.Workspace is created,
// mounted, kept after the call, and reused by the next call.
func TestPersistentWorkspaceSurvivesRuns(t *testing.T) {
	cfg := testConfig(t)
	ws := filepath.Join(t.TempDir(), "u1", "c1")
	f := &fakeExec{}
	r := newTestRunner(cfg, f)

	f.fn = writeViaRun("first.txt", "one")
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x", Workspace: ws}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "first.txt")); err != nil || string(data) != "one" {
		t.Fatalf("workspace did not keep first.txt: %q, %v", data, err)
	}
	info, err := os.Stat(ws)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Errorf("workspace mode = %o, want 0777 (container user must write)", info.Mode().Perm())
	}
	// Age run 1's file past the one-second harvest grace so run 2 is only
	// allowed to harvest what it writes itself.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(ws, "first.txt"), past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Run 2 must see run 1's file and mount the same host directory.
	f.fn = func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "run" {
			dir := strings.TrimSuffix(argValue(args, "-v"), ":/work:rw")
			if _, err := os.Stat(filepath.Join(dir, "first.txt")); err != nil {
				t.Errorf("second run cannot see first.txt: %v", err)
			}
		}
		return writeViaRun("second.txt", "two")(ctx, name, args, stdin)
	}
	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "y", Workspace: ws})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "second.txt" {
		t.Fatalf("second run artifacts = %+v, want only second.txt", res.Artifacts)
	}

	runs := f.find("run")
	if len(runs) != 2 {
		t.Fatalf("run calls = %d, want 2", len(runs))
	}
	for _, c := range runs {
		if got := strings.TrimSuffix(argValue(c.args, "-v"), ":/work:rw"); got != ws {
			t.Errorf("mount = %q, want %q", got, ws)
		}
		if got := ulimitValue(c.args, "fsize"); got != cfg.AgentFileSizeLimit {
			t.Errorf("fsize = %q, want agent limit %q", got, cfg.AgentFileSizeLimit)
		}
	}
}

// TestPersistentWorkspaceHarvestsOnlyNewFiles checks the mtime filter: a file
// already in the workspace is not re-delivered, and is not deleted either.
func TestPersistentWorkspaceHarvestsOnlyNewFiles(t *testing.T) {
	cfg := testConfig(t)
	ws := t.TempDir()
	old := filepath.Join(ws, "old.txt")
	writeFile(t, old, []byte("old"), 0o644)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	f := &fakeExec{}
	r := newTestRunner(cfg, f)
	f.fn = writeViaRun("new.txt", "new")
	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x", Workspace: ws})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "new.txt" {
		t.Fatalf("artifacts = %+v, want only new.txt", res.Artifacts)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("old workspace file removed: %v", err)
	}
}

// TestEphemeralStillHarvestsEverything guards that the mtime filter applies to
// persistent workspaces only.
func TestEphemeralStillHarvestsEverything(t *testing.T) {
	cfg := testConfig(t)
	f := &fakeExec{}
	r := newTestRunner(cfg, f)
	f.fn = func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "run" {
			dir := strings.TrimSuffix(argValue(args, "-v"), ":/work:rw")
			write := filepath.Join(dir, "stale.txt")
			if err := os.WriteFile(write, []byte("x"), 0o644); err != nil {
				return nil, nil, err
			}
			past := time.Now().Add(-time.Hour)
			_ = os.Chtimes(write, past, past)
		}
		return nil, nil, nil
	}
	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "stale.txt" {
		t.Fatalf("artifacts = %+v, want stale.txt", res.Artifacts)
	}
}

// TestSweepLeftoversSkipsAgentWorkspaces checks the one-hour leftover sweep
// never touches persistent agent workspaces.
func TestSweepLeftoversSkipsAgentWorkspaces(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t)
	cfg.WorkDir = root
	r := NewRunner(cfg)

	conv := filepath.Join(root, AgentWorkspaceDirName, "u1", "c1")
	if err := os.MkdirAll(conv, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(conv, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	r.SweepLeftovers()
	if _, err := os.Stat(conv); err != nil {
		t.Fatalf("SweepLeftovers deleted %s: %v", conv, err)
	}
}

// TestSweepAgentWorkspacesIdle checks only conversation dirs older than idle go,
// and user dirs are removed once empty.
func TestSweepAgentWorkspacesIdle(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t)
	cfg.WorkDir = root
	r := NewRunner(cfg)

	agentRoot := filepath.Join(root, AgentWorkspaceDirName)
	oldConv := filepath.Join(agentRoot, "u1", "old")
	freshConv := filepath.Join(agentRoot, "u1", "fresh")
	otherOld := filepath.Join(agentRoot, "u2", "old")
	for _, d := range []string{oldConv, freshConv, otherOld} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, d := range []string{oldConv, otherOld} {
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	n, err := r.SweepAgentWorkspaces(time.Hour)
	if err != nil {
		t.Fatalf("SweepAgentWorkspaces: %v", err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}
	for _, d := range []string{oldConv, otherOld} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err=%v)", d, err)
		}
	}
	if _, err := os.Stat(freshConv); err != nil {
		t.Errorf("fresh workspace removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentRoot, "u2")); !os.IsNotExist(err) {
		t.Errorf("empty user dir u2 not removed")
	}

	// A missing agent root is not an error.
	empty := testConfig(t)
	empty.WorkDir = t.TempDir()
	if n, err := NewRunner(empty).SweepAgentWorkspaces(time.Hour); n != 0 || err != nil {
		t.Errorf("missing root: n=%d err=%v", n, err)
	}
}

// TestQuotaRefusesWhenOver checks the call is refused before anything runs.
func TestQuotaRefusesWhenOver(t *testing.T) {
	f := &fakeExec{}
	r := newTestRunner(testConfig(t), f)
	quotaDir := t.TempDir()
	writeFile(t, filepath.Join(quotaDir, "big.bin"), make([]byte, 100), 0o644)

	_, err := r.Run(context.Background(), Spec{
		Language: LanguageBash, Code: "x",
		QuotaDir: quotaDir, QuotaBytes: 50,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	if got := len(f.find("run")); got != 0 {
		t.Errorf("docker run calls = %d, want 0 (refused before exec)", got)
	}
	msg := err.Error()
	for _, want := range []string{"delete files in /work", "100 B", "50 B"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestQuotaAllowsUnderAndCaches checks an under-limit call runs and that the
// measurement is cached, so growth within the TTL is not re-walked.
func TestQuotaAllowsUnderAndCaches(t *testing.T) {
	f := &fakeExec{}
	r := newTestRunner(testConfig(t), f)
	quotaDir := t.TempDir()
	writeFile(t, filepath.Join(quotaDir, "small.bin"), make([]byte, 10), 0o644)
	spec := Spec{Language: LanguageBash, Code: "x", QuotaDir: quotaDir, QuotaBytes: 1000}

	if _, err := r.Run(context.Background(), spec); err != nil {
		t.Fatalf("under-limit run: %v", err)
	}
	// Grow well past the quota: a re-walk would now refuse, the cache must not.
	writeFile(t, filepath.Join(quotaDir, "grown.bin"), make([]byte, 2000), 0o644)
	if _, err := r.Run(context.Background(), spec); err != nil {
		t.Fatalf("second run re-walked (cache miss): %v", err)
	}
	if got := len(f.find("run")); got != 2 {
		t.Errorf("docker run calls = %d, want 2", got)
	}
}

// TestSpecTimeoutOverridesConfig checks Spec.Timeout reaches the context
// deadline and the error reports it.
func TestSpecTimeoutOverridesConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Timeout = time.Hour
	f := &fakeExec{}
	r := newTestRunner(cfg, f)
	f.fn = func(ctx context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "run" {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return nil, nil, nil
	}

	start := time.Now()
	_, err := r.Run(context.Background(), Spec{
		Language: LanguageBash, Code: "sleep", Timeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("run took %s, Spec.Timeout was not applied", elapsed)
	}
	if !strings.Contains(err.Error(), "50ms") {
		t.Errorf("timeout error %q does not report the per-call limit", err)
	}
}

// TestFileSizeLimitByMode checks ephemeral and persistent runs get different
// --ulimit fsize values.
func TestFileSizeLimitByMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.FileSizeLimit = "10485760:10485760"
	cfg.AgentFileSizeLimit = "2147483648:2147483648"
	f := &fakeExec{}
	r := newTestRunner(cfg, f)

	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"}); err != nil {
		t.Fatalf("ephemeral run: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x", Workspace: ws}); err != nil {
		t.Fatalf("workspace run: %v", err)
	}
	runs := f.find("run")
	if len(runs) != 2 {
		t.Fatalf("run calls = %d, want 2", len(runs))
	}
	if got := ulimitValue(runs[0].args, "fsize"); got != "10485760:10485760" {
		t.Errorf("ephemeral fsize = %q, want 10485760:10485760", got)
	}
	if got := ulimitValue(runs[1].args, "fsize"); got != "2147483648:2147483648" {
		t.Errorf("agent fsize = %q, want 2147483648:2147483648", got)
	}
}

// TestAgentFileSizeLimitIsNumeric checks the effective agent --ulimit fsize is
// plain soft:hard byte counts. Docker's --ulimit parser takes integers only, so
// a suffixed value like "2g:2g" makes every agent call fail with exit 125.
func TestAgentFileSizeLimitIsNumeric(t *testing.T) {
	f := &fakeExec{}
	r := newTestRunner(testConfig(t), f)
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x", Workspace: ws}); err != nil {
		t.Fatalf("run: %v", err)
	}
	runs := f.find("run")
	if len(runs) != 1 {
		t.Fatalf("run calls = %d, want 1", len(runs))
	}
	soft, hard, ok := strings.Cut(ulimitValue(runs[0].args, "fsize"), ":")
	if !ok {
		t.Fatalf("fsize = %q, want soft:hard", ulimitValue(runs[0].args, "fsize"))
	}
	for _, half := range []string{soft, hard} {
		if _, err := strconv.ParseInt(half, 10, 64); err != nil {
			t.Errorf("fsize half %q is not an integer: %v", half, err)
		}
	}
}

// TestInvalidFileSizeLimitCorrected checks a suffixed fsize value is replaced by
// its default (with a warning) rather than reaching docker and breaking calls.
func TestInvalidFileSizeLimitCorrected(t *testing.T) {
	cfg := testConfig(t)
	cfg.FileSizeLimit = "10m:10m"
	cfg.AgentFileSizeLimit = "2g:2g"
	f := &fakeExec{}
	r := newTestRunner(cfg, f)

	if got := r.Config().FileSizeLimit; got != DefaultFileSizeLimit {
		t.Errorf("ephemeral fsize = %q, want default %q", got, DefaultFileSizeLimit)
	}
	if got := r.Config().AgentFileSizeLimit; got != DefaultAgentFileSizeLimit {
		t.Errorf("agent fsize = %q, want default %q", got, DefaultAgentFileSizeLimit)
	}
	// The corrected values are what actually reach docker.
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"}); err != nil {
		t.Fatalf("ephemeral run: %v", err)
	}
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x", Workspace: filepath.Join(t.TempDir(), "ws")}); err != nil {
		t.Fatalf("agent run: %v", err)
	}
	runs := f.find("run")
	if len(runs) != 2 {
		t.Fatalf("run calls = %d, want 2", len(runs))
	}
	for i, want := range []string{DefaultFileSizeLimit, DefaultAgentFileSizeLimit} {
		if got := ulimitValue(runs[i].args, "fsize"); got != want {
			t.Errorf("run %d fsize = %q, want %q", i, got, want)
		}
	}
}

// TestLimitedBufCapsCapture checks the raw capture cannot grow past its limit
// and still consumes writes so the child process never blocks.
func TestLimitedBufCapsCapture(t *testing.T) {
	w := &limitedBuf{limit: 10}
	if n, err := w.Write([]byte(strings.Repeat("x", 1000))); n != 1000 || err != nil {
		t.Fatalf("Write = %d, %v; want 1000, nil", n, err)
	}
	if w.buf.Len() != 10 || !w.truncated {
		t.Fatalf("buffered = %d, truncated = %v; want 10, true", w.buf.Len(), w.truncated)
	}
	if n, _ := w.Write([]byte("more")); n != 4 {
		t.Errorf("Write after full = %d, want 4 (discarded)", n)
	}
	if w.buf.Len() != 10 {
		t.Errorf("buffer grew to %d after limit", w.buf.Len())
	}

	u := &limitedBuf{} // limit 0 = unbounded
	_, _ = u.Write([]byte(strings.Repeat("y", 100)))
	if u.buf.Len() != 100 || u.truncated {
		t.Errorf("unbounded buffer = %d, truncated=%v", u.buf.Len(), u.truncated)
	}

	if got := captureLimit(100); got != 400 {
		t.Errorf("captureLimit(100) = %d, want 400", got)
	}
	if got := captureLimit(0); got != 0 {
		t.Errorf("captureLimit(0) = %d, want 0 (unbounded)", got)
	}
}

// TestDirSizeAndFreeBytes covers the two helpers the server quota UI needs.
func TestDirSizeAndFreeBytes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), make([]byte, 5), 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "sub", "b"), make([]byte, 7), 0o644)
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	size, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if size != 12 {
		t.Errorf("DirSize = %d, want 12 (%v)", size, size)
	}
	if size, err := DirSize(filepath.Join(dir, "missing")); err != nil || size != 0 {
		t.Errorf("DirSize(missing) = %d, %v; want 0, nil", size, err)
	}
	free, err := FreeBytes(dir)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if free == 0 {
		t.Error("FreeBytes = 0")
	}
}

func TestAgentQuotaFromEnv(t *testing.T) {
	t.Setenv("MYOGCHAT_AGENT_QUOTA_BYTES", "1024")
	if got := ConfigFromEnv().AgentQuotaBytes; got != 1024 {
		t.Errorf("AgentQuotaBytes = %d, want 1024", got)
	}
	t.Setenv("MYOGCHAT_AGENT_QUOTA_BYTES", "bogus")
	if got := ConfigFromEnv().AgentQuotaBytes; got != DefaultAgentQuotaBytes {
		t.Errorf("invalid env: AgentQuotaBytes = %d, want default %d", got, DefaultAgentQuotaBytes)
	}
}
