package extract

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// buildPDF constructs a minimal, valid single-page PDF whose content stream
// draws text with Helvetica. It is generated (not embedded) so the fixture
// stays tiny and reviewable while exercising the real parser.
func buildPDF(text string) []byte {
	escaped := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(text)
	content := "BT /F1 24 Tf 72 720 Td (" + escaped + ") Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xrefPos)
	return buf.Bytes()
}

func TestExtractPlainTextStripsNULs(t *testing.T) {
	got, err := Extract("notes.txt", "text/plain; charset=utf-8", []byte("hello\x00 world"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestExtractRejectsInvalidUTF8(t *testing.T) {
	_, err := Extract("bad.txt", "text/plain", []byte{0xff, 0xfe, 0xfd})
	if !errors.Is(err, ErrNotText) {
		t.Errorf("err = %v, want ErrNotText", err)
	}
}

func TestExtractByExtensionWhenMimeMissing(t *testing.T) {
	got, err := Extract("config.json", "", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestExtractImageUnsupported(t *testing.T) {
	_, err := Extract("photo.png", "image/png", []byte("not really an image"))
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestExtractBinaryUnsupported(t *testing.T) {
	_, err := Extract("archive.zip", "application/zip", []byte("PK\x03\x04binary"))
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestExtractPDF(t *testing.T) {
	got, err := Extract("sample.pdf", "application/pdf", buildPDF("Hello PDF"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "Hello PDF") {
		t.Errorf("extracted %q, want it to contain %q", got, "Hello PDF")
	}
}

func TestExtractPDFByMagicBytes(t *testing.T) {
	// A generic MIME type must still route a PDF to the PDF parser.
	got, err := Extract("download", "application/octet-stream", buildPDF("Magic"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "Magic") {
		t.Errorf("extracted %q, want it to contain %q", got, "Magic")
	}
}

func TestExtractCapsText(t *testing.T) {
	long := strings.Repeat("a", MaxTextChars+50)
	got, err := Extract("big.txt", "text/plain", []byte(long))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.HasSuffix(got, TruncationMarker) {
		t.Errorf("expected truncation marker at the end")
	}
	if n := len([]rune(got)); n != MaxTextChars+len([]rune(TruncationMarker)) {
		t.Errorf("capped length = %d, want %d", n, MaxTextChars+len([]rune(TruncationMarker)))
	}
}

func TestExtractEmpty(t *testing.T) {
	if _, err := Extract("empty.txt", "text/plain", nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("err = %v, want ErrEmpty", err)
	}
}
