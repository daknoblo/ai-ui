package docparse

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildZIP creates an archive from a name/content map.
func buildZIP(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// TestDOCXSkipsDeletedText is the regression test for tracked changes: the text
// a revision removed must never reach the model as if it were still there.
func TestDOCXSkipsDeletedText(t *testing.T) {
	data := buildDOCX(t, `<w:document><w:body><w:p>`+
		`<w:r><w:t>Price is 100 EUR</w:t></w:r>`+
		`<w:del><w:r><w:delText>Price is 999 EUR</w:delText></w:r></w:del>`+
		`</w:p></w:body></w:document>`)

	got, err := Extract("contract.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(got, "999") {
		t.Errorf("deleted text leaked into the extract: %q", got)
	}
	if !strings.Contains(got, "100") {
		t.Errorf("the surviving text is missing: %q", got)
	}
}

// TestDOCXSkipsFieldCodes makes sure field instructions stay out of the text.
func TestDOCXSkipsFieldCodes(t *testing.T) {
	data := buildDOCX(t, `<w:document><w:body><w:p>`+
		`<w:r><w:instrText> HYPERLINK "https://example.com/tracking" </w:instrText></w:r>`+
		`<w:r><w:t>Documentation</w:t></w:r></w:p></w:body></w:document>`)

	got, err := Extract("doc.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(got, "HYPERLINK") {
		t.Errorf("a field instruction leaked into the extract: %q", got)
	}
	if !strings.Contains(got, "Documentation") {
		t.Errorf("the visible text is missing: %q", got)
	}
}

// TestDOCXIgnoresIndentation covers a pretty printed part: the whitespace
// between the elements is markup, not text.
func TestDOCXIgnoresIndentation(t *testing.T) {
	data := buildDOCX(t, "<w:document>\n\t<w:body>\n\t\t<w:p>\n\t\t\t<w:r>\n\t\t\t\t"+
		"<w:t>One</w:t>\n\t\t\t</w:r>\n\t\t</w:p>\n\t\t<w:p>\n\t\t\t<w:r>\n\t\t\t\t"+
		"<w:t>Two</w:t>\n\t\t\t</w:r>\n\t\t</w:p>\n\t</w:body>\n</w:document>")

	got, err := Extract("doc.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != "One\nTwo" {
		t.Errorf("extracted text = %q, want %q", got, "One\nTwo")
	}
}

// TestDOCXKeepsTableRows verifies a table survives as a table: cells separated
// by tabs, one row per line.
func TestDOCXKeepsTableRows(t *testing.T) {
	data := buildDOCX(t, `<w:document><w:body><w:tbl>`+
		`<w:tr><w:tc><w:p><w:r><w:t>Quarter</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Revenue</w:t></w:r></w:p></w:tc></w:tr>`+
		`<w:tr><w:tc><w:p><w:r><w:t>Q3</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>1200</w:t></w:r></w:p></w:tc></w:tr>`+
		`</w:tbl></w:body></w:document>`)

	got, err := Extract("table.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one per row: %q", len(lines), got)
	}
	if lines[0] != "Quarter\tRevenue" || lines[1] != "Q3\t1200" {
		t.Errorf("the rows lost their cell separation: %q", got)
	}
}

// TestDOCXReadsHeadersAndFootnotes checks the surrounding parts are included.
func TestDOCXReadsHeadersAndFootnotes(t *testing.T) {
	body := `<w:document><w:body><w:p><w:r><w:t>Body text</w:t></w:r></w:p></w:body></w:document>`
	header := `<w:hdr><w:p><w:r><w:t>ACME Ltd</w:t></w:r></w:p></w:hdr>`
	notes := `<w:footnotes><w:footnote><w:p><w:r><w:t>Excluding VAT</w:t></w:r></w:p></w:footnote></w:footnotes>`

	data := buildZIP(t, map[string]string{
		"word/document.xml":  body,
		"word/header1.xml":   header,
		"word/footnotes.xml": notes,
	})

	got, err := Extract("letter.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"Body text", "ACME Ltd", "Excluding VAT"} {
		if !strings.Contains(got, want) {
			t.Errorf("extract is missing %q: %q", want, got)
		}
	}
}

// TestXLSXResolvesSharedStrings covers the workbook path: cells reference a
// shared string table, so without resolving it a sheet reads as bare numbers.
func TestXLSXResolvesSharedStrings(t *testing.T) {
	data := buildZIP(t, map[string]string{
		"xl/workbook.xml": `<workbook><sheets><sheet name="Figures" sheetId="1"/></sheets></workbook>`,
		"xl/sharedStrings.xml": `<sst><si><t>Quarter</t></si><si><t>Revenue</t></si>` +
			`<si><r><t>Q3 </t></r><r><t>2025</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData>` +
			`<row><c t="s"><v>0</v></c><c t="s"><v>1</v></c></row>` +
			`<row><c t="s"><v>2</v></c><c><v>1234.5</v></c></row>` +
			`</sheetData></worksheet>`,
	})

	got, err := Extract("report.xlsx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"Figures", "Quarter\tRevenue", "Q3 2025\t1234.5"} {
		if !strings.Contains(got, want) {
			t.Errorf("extract is missing %q: %q", want, got)
		}
	}
}

// TestPPTXReadsSlidesInOrder makes sure slide 2 does not sort after slide 10 and
// the speaker notes come along.
func TestPPTXReadsSlidesInOrder(t *testing.T) {
	slide := func(text string) string {
		return `<p:sld><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>` +
			text + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
	}
	data := buildZIP(t, map[string]string{
		"ppt/slides/slide1.xml":  slide("First"),
		"ppt/slides/slide2.xml":  slide("Second"),
		"ppt/slides/slide10.xml": slide("Tenth"),
		"ppt/notesSlides/notesSlide1.xml": `<p:notes><p:cSld><p:spTree><p:sp><p:txBody>` +
			`<a:p><a:r><a:t>Remember the budget</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:notes>`,
	})

	got, err := Extract("deck.pptx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	first, second, tenth := strings.Index(got, "First"), strings.Index(got, "Second"), strings.Index(got, "Tenth")
	if first < 0 || second < 0 || tenth < 0 {
		t.Fatalf("a slide is missing: %q", got)
	}
	if first > second || second > tenth {
		t.Errorf("slides are out of order: %q", got)
	}
	if !strings.Contains(got, "Remember the budget") {
		t.Errorf("the speaker notes are missing: %q", got)
	}
}

// TestParseRTF covers the three things an RTF writer does that a naive strip
// gets wrong: ignorable destinations, the backslash line break and code page
// escapes.
func TestParseRTF(t *testing.T) {
	rtf := `{\rtf1\ansi\ansicpg1252` + "\n" +
		`{\fonttbl\f0\fswiss\fcharset0 Helvetica;}` + "\n" +
		`{\colortbl;\red255\green255\blue255;}` + "\n" +
		`{\*\expandedcolortbl;;}` + "\n" +
		`\pard\f0\fs24 \cf0 Gr\'fc\'df Gott\` + "\n" +
		`Second line\` + "\n" +
		`}`

	got, err := Extract("letter.rtf", "", []byte(rtf))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "Grüß Gott") {
		t.Errorf("code page escapes were not decoded: %q", got)
	}
	if !strings.Contains(got, "\nSecond line") {
		t.Errorf("the hard return was lost: %q", got)
	}
	if strings.ContainsAny(got, ";") || strings.Contains(got, "Helvetica") {
		t.Errorf("markup from a skipped destination leaked in: %q", got)
	}
}

// TestParseMarkup checks that scripts and styles stay out of the text while the
// block structure survives.
func TestParseMarkup(t *testing.T) {
	html := `<!DOCTYPE html><html><head><style>body{color:red}</style>` +
		`<script>var secret=1;</script></head><body>` +
		`<h1>Title</h1><p>First &amp; second.</p><ul><li>A</li><li>B</li></ul></body></html>`

	got, err := Extract("page.html", "text/html", []byte(html))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, unwanted := range []string{"color:red", "var secret"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("invisible content leaked in: %q", got)
		}
	}
	if !strings.Contains(got, "First & second.") {
		t.Errorf("entities were not decoded: %q", got)
	}
	if !strings.Contains(got, "Title") || !strings.Contains(got, "A") {
		t.Errorf("visible text is missing: %q", got)
	}
}

// TestExtractSniffsUnknownNames is what makes "attach anything" work: a file
// without a useful name or content type is still identified by its bytes.
func TestExtractSniffsUnknownNames(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		mime     string
		data     []byte
		want     string
	}{
		{"plain text without extension", "notes", "application/octet-stream",
			[]byte("Just some notes.\n"), "Just some notes."},
		{"source file", "main.go", "application/octet-stream",
			[]byte("package main\n"), "package main"},
		{"csv", "figures.csv", "", []byte("a,b\n1,2\n"), "a,b"},
		{"yaml", "config.yaml", "", []byte("key: value\n"), "key: value"},
		{"rtf by content", "unnamed", "application/octet-stream",
			[]byte(`{\rtf1\ansi Hello}`), "Hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Extract(tc.filename, tc.mime, tc.data)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("extract = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestExtractRejectsBinary makes sure the sniffing fallback does not turn an
// arbitrary binary into garbage text.
func TestExtractRejectsBinary(t *testing.T) {
	data := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x03}
	if _, err := Extract("blob.bin", "application/octet-stream", data); err == nil {
		t.Fatal("expected an error for binary content")
	}
}

// TestExtractExplainsLegacyOffice covers the pre-2007 formats: they cannot be
// parsed, so the message has to say what to do instead.
func TestExtractExplainsLegacyOffice(t *testing.T) {
	// D0 CF 11 E0 is the OLE2 compound file header.
	data := append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 64)...)
	_, err := Extract("old.doc", "application/msword", data)
	if err == nil {
		t.Fatal("expected an error for a legacy .doc")
	}
	if !strings.Contains(err.Error(), ".docx") {
		t.Errorf("the error does not name the way out: %v", err)
	}
}

// TestUploadAcceptCoversTheParsers keeps the file picker in sync with what the
// parsers actually handle.
func TestUploadAcceptCoversTheParsers(t *testing.T) {
	accept := UploadAccept()
	for _, ext := range []string{".pdf", ".docx", ".xlsx", ".pptx", ".rtf", ".md", ".csv", ".png"} {
		if !strings.Contains(accept, ext) {
			t.Errorf("the picker does not offer %s: %s", ext, accept)
		}
	}
	// A legacy format must not look supported.
	for _, ext := range []string{".doc,", ".xls,", ".ppt,"} {
		if strings.Contains(accept+",", ext) {
			t.Errorf("the picker offers the unsupported %s: %s", ext, accept)
		}
	}
}
