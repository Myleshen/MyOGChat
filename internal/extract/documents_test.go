package extract

import (
	"archive/zip"
	"bytes"
	"sort"
	"strings"
	"testing"
)

// zipBytes builds an in-memory ZIP from name→content (deterministic order).
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatalf("zip create %s: %v", n, err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatalf("zip write %s: %v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDOCX(t *testing.T) {
	data := zipBytes(t, map[string]string{
		"[Content_Types].xml": `<Types/>`,
		"word/document.xml": `<?xml version="1.0"?>
<w:document xmlns:w="http://x"><w:body>
<w:p><w:r><w:t>Hello world</w:t></w:r></w:p>
<w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p>
</w:body></w:document>`,
	})
	got, err := ExtractWith("report.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", data, nil)
	if err != nil {
		t.Fatalf("ExtractWith(docx): %v", err)
	}
	if !strings.Contains(got, "Hello world") || !strings.Contains(got, "Second paragraph") {
		t.Errorf("docx text = %q", got)
	}
	if !strings.Contains(got, "Hello world\nSecond paragraph") {
		t.Errorf("docx paragraphs not separated by newline: %q", got)
	}
}

func TestExtractXLSX(t *testing.T) {
	data := zipBytes(t, map[string]string{
		"xl/workbook.xml": `<workbook/>`,
		"xl/sharedStrings.xml": `<sst><si><t>Name</t></si><si><t>Alice</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row>
<c t="s"><v>0</v></c><c t="s"><v>1</v></c></row></sheetData></worksheet>`,
	})
	got, err := ExtractWith("sheet.xlsx", "", data, nil)
	if err != nil {
		t.Fatalf("ExtractWith(xlsx): %v", err)
	}
	if !strings.Contains(got, "Name") || !strings.Contains(got, "Alice") {
		t.Errorf("xlsx text = %q", got)
	}
}

func TestExtractPPTX(t *testing.T) {
	data := zipBytes(t, map[string]string{
		"ppt/presentation.xml": `<p:presentation xmlns:p="x"/>`,
		"ppt/slides/slide2.xml": `<p:sld xmlns:p="x" xmlns:a="y"><p:cSld><p:spTree><p:sp><p:txBody>
<a:p><a:r><a:t>Slide two</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide1.xml": `<p:sld xmlns:p="x" xmlns:a="y"><p:cSld><p:spTree><p:sp><p:txBody>
<a:p><a:r><a:t>Slide one</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	})
	got, err := ExtractWith("deck.pptx", "", data, nil)
	if err != nil {
		t.Fatalf("ExtractWith(pptx): %v", err)
	}
	if !strings.Contains(got, "Slide one") || !strings.Contains(got, "Slide two") {
		t.Errorf("pptx text = %q", got)
	}
	if strings.Index(got, "Slide one") > strings.Index(got, "Slide two") {
		t.Errorf("pptx slides out of order: %q", got)
	}
}

func TestExtractODT(t *testing.T) {
	data := zipBytes(t, map[string]string{
		"mimetype":    "application/vnd.oasis.opendocument.text",
		"content.xml": `<office:document-content xmlns:office="o" xmlns:text="t"><office:body><office:text><text:p>Report body</text:p></office:text></office:body></office:document-content>`,
	})
	got, err := ExtractWith("doc.odt", "", data, nil)
	if err != nil {
		t.Fatalf("ExtractWith(odt): %v", err)
	}
	if !strings.Contains(got, "Report body") {
		t.Errorf("odt text = %q", got)
	}
}

func TestExtractEPUB(t *testing.T) {
	data := zipBytes(t, map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="OEBPS/content.opf"/></rootfiles></container>`,
		"OEBPS/content.opf": `<package><manifest>
<item id="c1" href="chap1.xhtml" media-type="application/xhtml+xml"/></manifest>
<spine><itemref idref="c1"/></spine></package>`,
		"OEBPS/chap1.xhtml": `<html><head><style>p{color:red}</style></head><body>
<h1>Chapter One</h1><p>Once &amp; upon a time.</p></body></html>`,
	})
	got, err := ExtractWith("book.epub", "", data, nil)
	if err != nil {
		t.Fatalf("ExtractWith(epub): %v", err)
	}
	if !strings.Contains(got, "Chapter One") || !strings.Contains(got, "Once & upon a time.") {
		t.Errorf("epub text = %q", got)
	}
	if strings.Contains(got, "color:red") || strings.Contains(got, "<h1>") {
		t.Errorf("epub markup not stripped: %q", got)
	}
}

func TestExtractRTF(t *testing.T) {
	data := []byte(`{\rtf1\ansi Hello \b world\b0 !\par Second line with \'e9 accent}`)
	got, err := ExtractWith("note.rtf", "application/rtf", data, nil)
	if err != nil {
		t.Fatalf("ExtractWith(rtf): %v", err)
	}
	for _, want := range []string{"Hello", "world", "Second line", "é"} {
		if !strings.Contains(got, want) {
			t.Errorf("rtf text = %q, missing %q", got, want)
		}
	}
}

// TestDetectDocumentByMagic checks a document with no extension/MIME hint is
// still recognized from its bytes.
func TestDetectDocumentByMagic(t *testing.T) {
	docx := zipBytes(t, map[string]string{
		"word/document.xml": `<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Magic body</w:t></w:r></w:p></w:body></w:document>`,
	})
	got, err := ExtractWith("upload", "application/octet-stream", docx, nil)
	if err != nil {
		t.Fatalf("ExtractWith(magic docx): %v", err)
	}
	if !strings.Contains(got, "Magic body") {
		t.Errorf("magic docx text = %q", got)
	}
}

func TestStripHTML(t *testing.T) {
	got := stripHTML(`<div><p>One</p><p>Two &amp; three</p><script>bad()</script></div>`)
	if !strings.Contains(got, "One") || !strings.Contains(got, "Two & three") {
		t.Errorf("stripHTML = %q", got)
	}
	if strings.Contains(got, "bad()") {
		t.Errorf("stripHTML kept script: %q", got)
	}
}
