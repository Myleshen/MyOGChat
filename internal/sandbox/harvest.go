package sandbox

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// artifactMimeByExt maps well-known artifact extensions to a precise MIME type.
// Content sniffing is ambiguous for text formats (a CSV or Markdown file sniffs
// as text/plain), so a known extension wins; unknown extensions fall back to
// sniffing. The value is only metadata — the download endpoint re-checks it
// against its own allowlist and always sends the bytes as an attachment.
var artifactMimeByExt = map[string]string{
	".txt":  "text/plain",
	".csv":  "text/csv",
	".tsv":  "text/tab-separated-values",
	".md":   "text/markdown",
	".json": "application/json",
	".xml":  "application/xml",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
	".html": "text/html",
	".htm":  "text/html",
	".pdf":  "application/pdf",
	".zip":  "application/zip",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odt":  "application/vnd.oasis.opendocument.text",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".log":  "text/plain",
	".sql":  "application/sql",
	".sh":   "text/x-shellscript",
	".py":   "text/x-python",
	".go":   "text/x-go",
	".js":   "text/javascript",
}

// skipArtifactDirs are directory names never descended into during a harvest:
// Python bytecode caches, installed package trees and dependency dirs. Files in
// these are build/tooling byproducts, not things the user asked the agent to
// create (the sandbox sets HOME=/work, so pip/Python write caches there).
var skipArtifactDirs = map[string]bool{
	"__pycache__":   true,
	"site-packages": true,
	"dist-packages": true,
	"node_modules":  true,
}

// skipArtifactDir reports whether a directory should be pruned (cache/bytecode
// trees and any dot-directory such as .cache, .local or .git).
func skipArtifactDir(name string) bool {
	return skipArtifactDirs[name] || strings.HasPrefix(name, ".")
}

// skipArtifactFile reports whether a regular file is a tooling byproduct (Python
// bytecode, or a dotfile such as a lock/temp file).
func skipArtifactFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pyc", ".pyo", ".pyd":
		return true
	}
	return false
}

// harvest collects the regular files written under dir into artifacts. It is
// deliberately paranoid (5.8):
//
//   - Symlinks and every other non-regular entry are skipped, so a container
//     cannot link /work/out to a host file and exfiltrate it.
//   - Each file is opened with O_NOFOLLOW to close the Lstat/Open TOCTOU window,
//     and re-checked to be a regular file after opening.
//   - Tool caches and bytecode are pruned (see skipArtifactDir/File), so an
//     agent that pip-installs or imports a module does not flood the message
//     with hundreds of .pyc/cache blobs.
//   - Names are reduced to a sanitized base name (no directory components).
//   - Per-file, count and total caps bound what is stored.
//
// Files are returned in a deterministic (walk) order so tests are stable.
func harvest(dir string, cfg Config) []Artifact {
	if dir == "" {
		return nil
	}
	var (
		out     []Artifact
		total   int64
		used    = map[string]bool{}
		walkErr = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path == dir {
				return nil // skip an unreadable entry, never abort the harvest
			}
			if d.IsDir() {
				if skipArtifactDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				// Skip symlinks, devices and FIFOs. WalkDir does not descend
				// into symlinked directories.
				return nil
			}
			if skipArtifactFile(d.Name()) {
				return nil
			}
			if len(out) >= cfg.MaxFiles {
				return fs.SkipAll
			}
			art, size, ok := readArtifact(path, d.Name(), cfg)
			if !ok {
				return nil
			}
			if total+size > cfg.MaxTotalSize {
				return fs.SkipAll
			}
			art.Name = uniqueName(art.Name, used)
			out = append(out, art)
			total += size
			return nil
		})
	)
	_ = walkErr
	return out
}

// readArtifact opens one candidate file safely and returns its Artifact. ok is
// false for a symlink, a non-regular file, an unreadable or oversized file, or
// one whose name sanitizes away to nothing.
func readArtifact(path, base string, cfg Config) (Artifact, int64, bool) {
	name := sanitizeArtifactName(base)
	if name == "" {
		return Artifact{}, 0, false
	}
	// O_NOFOLLOW refuses to open a symlink; the kernel checks the final
	// component, so even a race between WalkDir and this Open cannot follow one.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Artifact{}, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Artifact{}, 0, false
	}
	if info.Size() > cfg.MaxFileSize {
		return Artifact{}, 0, false
	}
	data, err := io.ReadAll(io.LimitReader(f, cfg.MaxFileSize+1))
	if err != nil || int64(len(data)) > cfg.MaxFileSize {
		return Artifact{}, 0, false
	}
	return Artifact{Name: name, MIME: sniffMIME(name, data), Data: data}, int64(len(data)), true
}

// sniffMIME returns the artifact MIME type: a known extension wins (content
// sniffing reports text/plain for CSV/Markdown), otherwise the content is sniffed.
func sniffMIME(name string, data []byte) string {
	if m := artifactMimeByExt[strings.ToLower(filepath.Ext(name))]; m != "" {
		return m
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	ct := http.DetectContentType(head)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}

// sanitizeArtifactName reduces a file name to a safe base name: no directory
// components, no control characters, no surrounding dots/spaces, and a bounded
// length. It returns "" when nothing safe remains.
func sanitizeArtifactName(name string) string {
	name = filepath.Base(name) // removes any ../ traversal
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteRune('_')
		case unicode.IsControl(r):
			// drop
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		return ""
	}
	if len([]rune(out)) > 120 {
		r := []rune(out)
		ext := filepath.Ext(out)
		keep := 120 - len([]rune(ext))
		if keep < 1 {
			keep = 120
		}
		out = string(r[:keep]) + ext
	}
	return out
}

// uniqueName de-duplicates a name within one harvest by appending -2, -3, … so
// two files in different subdirectories do not collide in the store.
func uniqueName(name string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := stem + "-" + strconv.Itoa(i) + ext
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}
