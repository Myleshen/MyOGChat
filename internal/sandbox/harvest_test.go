package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func smallCapConfig() Config {
	c := DefaultConfig()
	c.MaxFileSize = 1024
	c.MaxTotalSize = 4096
	c.MaxFiles = 3
	return c
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestHarvestSkipsSymlinks checks that a symlink pointing at a host file is
// never read (the container must not be able to exfiltrate host data).
func TestHarvestSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, outside, []byte("TOP SECRET"), 0o644)
	writeFile(t, filepath.Join(dir, "ok.txt"), []byte("fine"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := harvest(dir, smallCapConfig())
	if len(got) != 1 || got[0].Name != "ok.txt" {
		t.Fatalf("harvest = %+v, want only ok.txt", got)
	}
	for _, a := range got {
		if strings.Contains(string(a.Data), "SECRET") {
			t.Fatalf("harvested host file through symlink: %+v", a)
		}
	}
}

// TestHarvestSanitizesTraversal checks names are reduced to a safe base name.
func TestHarvestSanitizesTraversal(t *testing.T) {
	cases := map[string]string{
		"..":               "",
		".":                "",
		"/etc/passwd":      "passwd",
		"../../etc/passwd": "passwd",
		"  spaced.txt  ":   "spaced.txt",
		".hidden":          "hidden",
		"a\x00b.txt":       "a_b.txt",
	}
	for in, want := range cases {
		if got := sanitizeArtifactName(in); got != want {
			t.Errorf("sanitizeArtifactName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHarvestUsesBaseNameForNestedFiles checks a file in a subdirectory is
// harvested under its base name (no directory components leak through).
func TestHarvestUsesBaseNameForNestedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "sub", "report.csv"), []byte("a,b\n1,2\n"), 0o644)
	got := harvest(dir, smallCapConfig())
	if len(got) != 1 || got[0].Name != "report.csv" {
		t.Fatalf("harvest = %+v, want report.csv", got)
	}
}

// TestHarvestPerFileCap checks an oversized file is skipped whole.
func TestHarvestPerFileCap(t *testing.T) {
	dir := t.TempDir()
	cfg := smallCapConfig()
	cfg.MaxFileSize = 8
	writeFile(t, filepath.Join(dir, "big.txt"), []byte("1234567890"), 0o644)
	writeFile(t, filepath.Join(dir, "small.txt"), []byte("ok"), 0o644)
	got := harvest(dir, cfg)
	if len(got) != 1 || got[0].Name != "small.txt" {
		t.Fatalf("harvest = %+v, want only small.txt", got)
	}
}

// TestHarvestCountAndTotalCaps checks the file-count and total-byte caps.
func TestHarvestCountAndTotalCaps(t *testing.T) {
	dir := t.TempDir()
	cfg := smallCapConfig()
	cfg.MaxFiles = 2
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, filepath.Join(dir, n), []byte("x"), 0o644)
	}
	if got := harvest(dir, cfg); len(got) != 2 {
		t.Fatalf("count cap: harvest = %d files, want 2", len(got))
	}

	dir2 := t.TempDir()
	cfg2 := smallCapConfig()
	cfg2.MaxTotalSize = 3
	writeFile(t, filepath.Join(dir2, "a.txt"), []byte("12"), 0o644)
	writeFile(t, filepath.Join(dir2, "b.txt"), []byte("34"), 0o644)
	if got := harvest(dir2, cfg2); len(got) != 1 {
		t.Fatalf("total cap: harvest = %d files, want 1", len(got))
	}
}

// TestHarvestMIME checks extension-based typing and content sniffing fallback.
func TestHarvestMIME(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "data.csv"), []byte("a,b\n1,2\n"), 0o644)
	writeFile(t, filepath.Join(dir, "notes.md"), []byte("# hi\n"), 0o644)
	// PNG magic bytes with a misleading extension: content sniffing wins.
	writeFile(t, filepath.Join(dir, "blob"), []byte("\x89PNG\r\n\x1a\nrest"), 0o644)
	writeFile(t, filepath.Join(dir, "sheet.xlsx"), []byte("PK\x03\x04zip"), 0o644)

	got := map[string]string{}
	cfg := smallCapConfig()
	cfg.MaxFiles = 10
	for _, a := range harvest(dir, cfg) {
		got[a.Name] = a.MIME
	}
	if got["data.csv"] != "text/csv" {
		t.Errorf("csv MIME = %q, want text/csv", got["data.csv"])
	}
	if got["notes.md"] != "text/markdown" {
		t.Errorf("md MIME = %q, want text/markdown", got["notes.md"])
	}
	if got["blob"] != "image/png" {
		t.Errorf("sniffed MIME = %q, want image/png", got["blob"])
	}
	if got["sheet.xlsx"] != artifactMimeByExt[".xlsx"] {
		t.Errorf("xlsx MIME = %q, want xlsx type", got["sheet.xlsx"])
	}
}

// TestHarvestDuplicateNames disambiguates two files with the same base name.
func TestHarvestDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFile(t, filepath.Join(dir, sub, "out.txt"), []byte(sub), 0o644)
	}
	got := harvest(dir, smallCapConfig())
	if len(got) != 2 {
		t.Fatalf("harvest = %d files, want 2", len(got))
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	if !names["out.txt"] || !names["out-2.txt"] {
		t.Fatalf("names = %v, want out.txt and out-2.txt", names)
	}
}

// TestRunHarvestsArtifacts drives a full run with a fake executor that writes a
// file into the bind-mounted host workspace, and checks it is harvested.
func TestRunHarvestsArtifacts(t *testing.T) {
	f := &fakeExec{}
	cfg := testConfig(t)
	r := newTestRunner(cfg, f)
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "run" {
			workDir := argValue(args, "-v")
			workDir = strings.TrimSuffix(workDir, ":/work:rw")
			_ = os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hi there"), 0o644)
			return []byte("done"), nil, nil
		}
		return nil, nil, nil
	}
	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "echo hi > hello.txt"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "hello.txt" {
		t.Fatalf("artifacts = %+v, want hello.txt", res.Artifacts)
	}
	if string(res.Artifacts[0].Data) != "hi there" {
		t.Errorf("artifact data = %q", res.Artifacts[0].Data)
	}
}

// TestRunCleansWorkspace checks the per-run directory is removed after a run.
func TestRunCleansWorkspace(t *testing.T) {
	f := &fakeExec{}
	cfg := testConfig(t)
	r := newTestRunner(cfg, f)
	var runDir string
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runDir = strings.TrimSuffix(argValue(args, "-v"), ":/work:rw")
		}
		return nil, nil, nil
	}
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("workspace %s still exists after run (err=%v)", runDir, err)
	}
}

// TestSweepLeftovers removes only workspaces older than the max age.
func TestSweepLeftovers(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t)
	cfg.WorkDir = root
	r := NewRunner(cfg)

	stale := filepath.Join(root, "stale-run")
	fresh := filepath.Join(root, "fresh-run")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	old := time.Now().Add(-2 * workspaceMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	r.SweepLeftovers()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale workspace not swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh workspace removed: %v", err)
	}
}
