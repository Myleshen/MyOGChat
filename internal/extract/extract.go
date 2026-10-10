// Package extract turns an uploaded file into the plain text folded into a
// model request (feature 2.3). Text/code/JSON/CSV/Markdown are decoded as UTF-8;
// PDFs are parsed with the pure-Go github.com/ledongthuc/pdf library.
// Documents (Phase 4.5) are handled by documents.go: OOXML (.docx/.xlsx/.pptx),
// Open Document (.odt/.ods/.odp), EPUB, and RTF, all via the standard library.
// Images are handled by OCR (feature 2.5): the extractor stays Docker-free by
// delegating to an injected ocr.OCRFunc.
package extract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"myogchat/internal/ocr"
)

const (
	// MaxTextChars caps the stored extracted text per attachment. The model
	// budget is enforced separately when folding (200k chars total).
	MaxTextChars = 1_000_000
	// TruncationMarker is appended when extracted text hits MaxTextChars.
	TruncationMarker = "\n\n[... text truncated ...]"
)

// Sentinel errors so the HTTP layer can map failures to clear messages.
var (
	// ErrUnsupported marks file types the extractor does not handle.
	ErrUnsupported = errors.New("unsupported file type")
	// ErrNotText marks files that are not valid UTF-8 text.
	ErrNotText = errors.New("file is not valid UTF-8 text")
	// ErrEmpty marks files that parsed but yielded no text.
	ErrEmpty = errors.New("no text could be extracted")
	// ErrOCR marks images whose OCR run failed (engine error, timeout, ...).
	ErrOCR = errors.New("OCR failed")
)

// textExtensions are filename extensions treated as UTF-8 text even when the
// browser reports a generic or empty MIME type.
var textExtensions = map[string]bool{
	".txt": true, ".text": true, ".md": true, ".markdown": true,
	".json": true, ".jsonl": true, ".csv": true, ".tsv": true,
	".xml": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".log": true, ".rst": true, ".tex": true, ".org": true,
	".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true,
	".ts": true, ".tsx": true, ".jsx": true, ".java": true, ".kt": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".cs": true, ".rs": true, ".rb": true, ".php": true, ".swift": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".sql": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".less": true,
	".vue": true, ".svelte": true, ".dart": true, ".lua": true, ".pl": true,
	".r": true, ".scala": true, ".clj": true, ".ex": true, ".exs": true,
	".erl": true, ".hs": true, ".ml": true, ".f": true, ".f90": true,
	".proto": true, ".graphql": true, ".gql": true, ".env": true,
	".conf": true, ".cfg": true, ".properties": true, ".diff": true, ".patch": true,
}

// imageExtensions are filename extensions treated as images (OCR) even when the
// browser reports a generic or empty MIME type.
var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true,
	".tif": true, ".tiff": true, ".webp": true, ".pbm": true, ".pgm": true,
	".ppm": true, ".pnm": true,
}

// Extract returns the plain text of data given the upload's filename and MIME
// type. It never returns both text and an error. Images are rejected with
// ErrUnsupported because no OCR function is supplied; use ExtractWith to OCR
// them.
func Extract(name, mimeType string, data []byte) (string, error) {
	return ExtractWith(name, mimeType, data, nil)
}

// ExtractWith is Extract with an injected OCR function used for images. A nil
// ocrFn keeps the pre-2.5 behavior: images are reported as unsupported. When
// ocrFn is set, recognized text is trimmed, capped with MaxTextChars, and
// returned like any other extractor output.
func ExtractWith(name, mimeType string, data []byte, ocrFn ocr.OCRFunc) (string, error) {
	base := baseMime(mimeType)
	ext := strings.ToLower(filepath.Ext(name))
	kind := detectDocument(base, ext, data)

	switch {
	case isPDF(base, ext, data):
		return extractPDF(data)
	case isImage(base, ext):
		return extractImage(data, ocrFn)
	case kind != "":
		return extractDocument(kind, data)
	case isText(base, ext, data):
		return extractText(data)
	default:
		if base == "" {
			base = "unknown"
		}
		return "", fmt.Errorf("%w: %s", ErrUnsupported, base)
	}
}

// baseMime strips any parameters (e.g. "text/plain; charset=utf-8").
func baseMime(m string) string {
	if m == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(m); err == nil {
		return strings.ToLower(parsed)
	}
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}

// isPDF detects PDFs by MIME type, extension, or the "%PDF" magic bytes.
func isPDF(base, ext string, data []byte) bool {
	return base == "application/pdf" || ext == ".pdf" || bytes.HasPrefix(data, []byte("%PDF"))
}

// isImage decides whether a non-PDF upload should be OCR'd. Images are matched
// by MIME type or, when the browser sends a generic type, by extension.
func isImage(base, ext string) bool {
	return strings.HasPrefix(base, "image/") || imageExtensions[ext]
}

// extractImage OCRs an image. A nil OCR function (OCR disabled or unavailable)
// yields ErrUnsupported so the HTTP layer can explain it; an OCR engine failure
// yields ErrOCR; an image with no recognizable text yields ErrEmpty.
func extractImage(data []byte, ocrFn ocr.OCRFunc) (string, error) {
	if ocrFn == nil {
		return "", fmt.Errorf("%w: images require OCR, which is disabled", ErrUnsupported)
	}
	if len(data) == 0 {
		return "", ErrEmpty
	}
	text, err := ocrFn(data)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrOCR, err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmpty
	}
	return capText(text), nil
}

// isText decides whether a non-PDF upload should be decoded as UTF-8 text.
func isText(base, ext string, data []byte) bool {
	switch {
	case strings.HasPrefix(base, "text/"):
		return true
	case base == "application/json", base == "application/xml", base == "application/x-yaml",
		base == "application/yaml", base == "application/toml", base == "application/javascript",
		base == "application/x-javascript", base == "application/sql",
		base == "application/x-sh", base == "application/x-httpd-php":
		return true
	case textExtensions[ext]:
		return true
	case base == "" || base == "application/octet-stream":
		// No usable MIME hint: accept only data that decodes cleanly as UTF-8
		// with no NUL bytes (a cheap "looks like text" test).
		return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
	default:
		return false
	}
}

// extractText validates UTF-8, strips NULs, and caps the result.
func extractText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", ErrNotText
	}
	clean := bytes.ReplaceAll(data, []byte{0}, nil)
	if len(clean) == 0 {
		return "", ErrEmpty
	}
	return capText(string(clean)), nil
}

// extractPDF reads all text from a PDF in memory. The library can panic on
// malformed input, so the call is guarded.
func extractPDF(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("%w: malformed PDF", ErrUnsupported)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read PDF: %w", err)
	}
	rd, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extract PDF text: %w", err)
	}
	// Bound the read: MaxTextChars runes are at most 4 bytes each, plus slack.
	raw, err := io.ReadAll(io.LimitReader(rd, int64(MaxTextChars)*4+16))
	if err != nil {
		return "", fmt.Errorf("extract PDF text: %w", err)
	}
	raw = bytes.ReplaceAll(raw, []byte{0}, nil)
	if !utf8.Valid(raw) {
		return "", ErrNotText
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", ErrEmpty
	}
	return capText(string(raw)), nil
}

// capText truncates s to MaxTextChars runes, appending a clear marker.
func capText(s string) string {
	r := []rune(s)
	if len(r) <= MaxTextChars {
		return s
	}
	return string(r[:MaxTextChars]) + TruncationMarker
}
