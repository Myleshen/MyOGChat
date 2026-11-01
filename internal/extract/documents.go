// Document-format extraction (Phase 4.5): OOXML (.docx/.xlsx/.pptx), Open
// Document (.odt/.ods/.odp), EPUB, and RTF. Everything here uses only the
// standard library (archive/zip + encoding/xml), so the binary stays CGO-free
// and cross-compilable to linux/arm64 with no new dependencies.
//
// The extractor walks the OOXML/ODF XML and collects character data, inserting
// line/tab breaks at paragraph/cell boundaries. EPUB is unpacked along its OPF
// spine and each XHTML document is tag-stripped. RTF is reduced with a small
// pragmatic stripper. None of these are byte-perfect renderers; the goal is
// faithful-enough plain text for the model.
package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// maxDocEntryBytes caps how much of any single archive entry we read.
	maxDocEntryBytes = 48 << 20
	// maxDocEntries bounds how many archive entries we consider (zip-bomb guard).
	maxDocEntries = 4096
)

// detectDocument classifies a non-text/non-PDF upload as a supported document
// format, or "" if it is not one. It uses the MIME type and extension first,
// then falls back to magic bytes.
func detectDocument(base, ext string, data []byte) string {
	switch ext {
	case ".docx":
		return "docx"
	case ".xlsx":
		return "xlsx"
	case ".pptx":
		return "pptx"
	case ".odt":
		return "odt"
	case ".ods":
		return "ods"
	case ".odp":
		return "odp"
	case ".epub":
		return "epub"
	case ".rtf":
		return "rtf"
	}
	switch base {
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	case "application/vnd.oasis.opendocument.text":
		return "odt"
	case "application/vnd.oasis.opendocument.spreadsheet":
		return "ods"
	case "application/vnd.oasis.opendocument.presentation":
		return "odp"
	case "application/epub+zip":
		return "epub"
	case "application/rtf", "text/rtf":
		return "rtf"
	}
	switch {
	case bytes.HasPrefix(data, []byte("{\\rtf")):
		return "rtf"
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return detectZipKind(data)
	}
	return ""
}

// detectZipKind inspects a ZIP's entry names (and, for ODF, its mimetype entry)
// to tell OOXML/ODF/EPUB apart.
func detectZipKind(data []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	switch {
	case names["word/document.xml"]:
		return "docx"
	case names["xl/workbook.xml"]:
		return "xlsx"
	case names["ppt/presentation.xml"]:
		return "pptx"
	case names["META-INF/container.xml"]:
		return "epub"
	case names["content.xml"] && names["mimetype"]:
		mt, _ := readZipEntry(zr, "mimetype")
		switch strings.TrimSpace(string(mt)) {
		case "application/vnd.oasis.opendocument.text":
			return "odt"
		case "application/vnd.oasis.opendocument.spreadsheet":
			return "ods"
		case "application/vnd.oasis.opendocument.presentation":
			return "odp"
		}
	}
	return ""
}

// extractDocument turns a detected document kind into plain text.
func extractDocument(kind string, data []byte) (string, error) {
	var (
		text string
		err  error
	)
	switch kind {
	case "docx":
		text, err = zipEntryText(data, "word/document.xml", "p", "tab")
	case "pptx":
		text, err = extractPptx(data)
	case "xlsx":
		text, err = extractXlsx(data)
	case "odt", "ods", "odp":
		text, err = zipEntryText(data, "content.xml", "p table-cell", "")
	case "epub":
		text, err = extractEPUB(data)
	case "rtf":
		text, err = extractRTF(data)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupported, kind)
	}
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmpty
	}
	return capText(text), nil
}

// newZip opens a ZIP archive from memory (nil on failure).
func newZip(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}

// readZipEntry reads one named entry, capped at maxDocEntryBytes.
func readZipEntry(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxDocEntryBytes))
	}
	return nil, fmt.Errorf("entry %q not found", name)
}

// zipEntryText reads one XML entry and extracts its text.
func zipEntryText(data []byte, name, breaks, tabs string) (string, error) {
	zr, err := newZip(data)
	if err != nil {
		return "", fmt.Errorf("%w: not a valid archive", ErrUnsupported)
	}
	raw, err := readZipEntry(zr, name)
	if err != nil {
		return "", fmt.Errorf("%w: missing %s", ErrUnsupported, name)
	}
	return xmlText(bytes.NewReader(raw), breaks, tabs), nil
}

// xmlText collects character data from an XML stream, inserting a newline when
// a break element ends and a tab when a tab element starts. Malformed tails are
// tolerated (we stop at the first decode error). Namespaces are ignored.
func xmlText(r io.Reader, breaks, tabs string) string {
	brk := fieldSet(breaks)
	tab := fieldSet(tabs)
	dec := xml.NewDecoder(r)
	var b strings.Builder
	limit := int64(MaxTextChars)*4 + 64
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.CharData:
			// Skip whitespace-only nodes (inter-tag formatting in pretty XML).
			if len(bytes.TrimSpace(t)) == 0 {
				continue
			}
			b.Write(t)
		case xml.StartElement:
			if tab[t.Name.Local] {
				b.WriteByte('\t')
			}
		case xml.EndElement:
			if brk[t.Name.Local] {
				b.WriteByte('\n')
			}
		}
		if int64(b.Len()) >= limit {
			break
		}
	}
	return b.String()
}

func fieldSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

// extractPptx reads every slide in numeric order.
func extractPptx(data []byte) (string, error) {
	zr, err := newZip(data)
	if err != nil {
		return "", fmt.Errorf("%w: not a valid archive", ErrUnsupported)
	}
	var slides []string
	for _, f := range zr.File {
		if matched, _ := path.Match("ppt/slides/slide*.xml", f.Name); matched {
			slides = append(slides, f.Name)
		}
	}
	sort.Slice(slides, func(i, j int) bool { return slideNum(slides[i]) < slideNum(slides[j]) })
	var b strings.Builder
	for _, name := range slides {
		raw, err := readZipEntry(zr, name)
		if err != nil {
			continue
		}
		b.WriteString(xmlText(bytes.NewReader(raw), "p", ""))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

var slideNumRE = regexp.MustCompile(`slide(\d+)\.xml$`)

func slideNum(name string) int {
	if m := slideNumRE.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 1 << 30
}

// extractXlsx reads the shared strings plus every worksheet. Shared strings hold
// the workbook's text; worksheets add numbers and inline strings.
func extractXlsx(data []byte) (string, error) {
	zr, err := newZip(data)
	if err != nil {
		return "", fmt.Errorf("%w: not a valid archive", ErrUnsupported)
	}
	var b strings.Builder
	if raw, err := readZipEntry(zr, "xl/sharedStrings.xml"); err == nil {
		b.WriteString(xmlText(bytes.NewReader(raw), "si", ""))
	}
	var sheets []string
	for _, f := range zr.File {
		if matched, _ := path.Match("xl/worksheets/sheet*.xml", f.Name); matched {
			sheets = append(sheets, f.Name)
		}
	}
	sort.Strings(sheets)
	for _, name := range sheets {
		raw, err := readZipEntry(zr, name)
		if err != nil {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(xmlText(bytes.NewReader(raw), "row", "c"))
	}
	return b.String(), nil
}

// --- EPUB ---

type opfContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfPackage struct {
	Manifest struct {
		Items []struct {
			ID        string `xml:"id,attr"`
			Href      string `xml:"href,attr"`
			MediaType string `xml:"media-type,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Itemrefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

// extractEPUB reads the XHTML documents in OPF spine order and strips markup.
func extractEPUB(data []byte) (string, error) {
	zr, err := newZip(data)
	if err != nil {
		return "", fmt.Errorf("%w: not a valid archive", ErrUnsupported)
	}
	opfPath := epubOPFPath(zr)
	if opfPath == "" {
		return epubFallback(zr), nil
	}
	raw, err := readZipEntry(zr, opfPath)
	if err != nil {
		return epubFallback(zr), nil
	}
	var pkg opfPackage
	if err := xml.Unmarshal(raw, &pkg); err != nil {
		return epubFallback(zr), nil
	}
	hrefByID := map[string]string{}
	for _, it := range pkg.Manifest.Items {
		hrefByID[it.ID] = it.Href
	}
	baseDir := path.Dir(opfPath)
	var b strings.Builder
	read := 0
	for _, ref := range pkg.Spine.Itemrefs {
		href, ok := hrefByID[ref.IDRef]
		if !ok {
			continue
		}
		name := resolveZipPath(baseDir, href)
		raw, err := readZipEntry(zr, name)
		if err != nil {
			continue
		}
		b.WriteString(stripHTML(string(raw)))
		b.WriteString("\n\n")
		read++
		if read >= maxDocEntries {
			break
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return epubFallback(zr), nil
	}
	return b.String(), nil
}

func epubOPFPath(zr *zip.Reader) string {
	raw, err := readZipEntry(zr, "META-INF/container.xml")
	if err != nil {
		return ""
	}
	var c opfContainer
	if err := xml.Unmarshal(raw, &c); err != nil || len(c.Rootfiles) == 0 {
		return ""
	}
	return c.Rootfiles[0].FullPath
}

// epubFallback concatenates any XHTML/HTML entries when the OPF is unusable.
func epubFallback(zr *zip.Reader) string {
	var names []string
	for _, f := range zr.File {
		ext := strings.ToLower(path.Ext(f.Name))
		if ext == ".xhtml" || ext == ".html" || ext == ".htm" {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		raw, err := readZipEntry(zr, name)
		if err != nil {
			continue
		}
		b.WriteString(stripHTML(string(raw)))
		b.WriteString("\n\n")
	}
	return b.String()
}

// resolveZipPath joins an OPF directory with a (possibly relative) href and
// cleans it, guarding against escaping the archive root.
func resolveZipPath(baseDir, href string) string {
	joined := path.Clean(path.Join(baseDir, href))
	return strings.TrimPrefix(joined, "/")
}

// --- HTML stripping ---

var (
	reScript     = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	reStyle      = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`)
	reBreakTags  = regexp.MustCompile(`(?i)</(p|div|br|h[1-6]|li|tr|section|article|blockquote|pre)\s*>|<br\s*/?>`)
	reAnyTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reWhitespace = regexp.MustCompile(`[ \t\f\v]+`)
	reBlankLines = regexp.MustCompile(`\n\s*\n\s*(\n\s*)+`)
	htmlEntities = map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`,
		"&#39;": "'", "&apos;": "'", "&nbsp;": " ", "&mdash;": "—",
		"&ndash;": "–", "&hellip;": "…", "&rsquo;": "’", "&lsquo;": "‘",
		"&ldquo;": "“", "&rdquo;": "”",
	}
)

// stripHTML reduces HTML/XHTML to readable text.
func stripHTML(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	s = reStyle.ReplaceAllString(s, " ")
	s = reBreakTags.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, "")
	s = decodeEntities(s)
	s = reWhitespace.ReplaceAllString(s, " ")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	// Trim trailing spaces on each line.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(ln)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	for ent, rep := range htmlEntities {
		s = strings.ReplaceAll(s, ent, rep)
	}
	// Numeric entities.
	s = numericEntityRE.ReplaceAllStringFunc(s, func(m string) string {
		body := strings.TrimSuffix(strings.TrimPrefix(m, "&#"), ";")
		base := 10
		if len(body) > 0 && (body[0] == 'x' || body[0] == 'X') {
			base, body = 16, body[1:]
		}
		n, err := strconv.ParseInt(body, base, 32)
		if err != nil || n <= 0 || n > 0x10FFFF {
			return m
		}
		return string(rune(n))
	})
	return s
}

var numericEntityRE = regexp.MustCompile(`&#[xX]?[0-9A-Fa-f]+;`)

// --- RTF ---

var (
	rtfControl  = regexp.MustCompile(`\\[a-zA-Z]+-?\d*[ ]?`)
	rtfHex      = regexp.MustCompile(`\\'[0-9A-Fa-f]{2}`)
	rtfUnicode  = regexp.MustCompile(`\\u(-?\d+)\??`)
	rtfNewlines = regexp.MustCompile(`\\(par|line)\b\s?`)
)

// extractRTF reduces RTF to plain text. It is intentionally pragmatic: control
// words are dropped, paragraph/line breaks become newlines, and hex escapes are
// decoded as Latin-1.
func extractRTF(data []byte) (string, error) {
	s := string(data)
	// Drop destinations and the RTF header group markers loosely.
	s = rtfNewlines.ReplaceAllString(s, "\n")
	s = rtfUnicode.ReplaceAllStringFunc(s, func(m string) string {
		sub := rtfUnicode.FindStringSubmatch(m)
		n, err := strconv.Atoi(sub[1])
		if err != nil || n < 0 {
			return ""
		}
		return string(rune(n))
	})
	s = rtfHex.ReplaceAllStringFunc(s, func(m string) string {
		b, err := strconv.ParseUint(m[2:], 16, 8)
		if err != nil {
			return ""
		}
		return string(rune(b))
	})
	s = rtfControl.ReplaceAllString(s, "")
	// Remove grouping braces and collapse.
	s = strings.NewReplacer("{", "", "}", "", "\r", "\n").Replace(s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n"), nil
}
