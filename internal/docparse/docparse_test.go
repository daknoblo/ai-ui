package docparse

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildDOCX creates a minimal .docx archive containing the given document.xml.
func buildDOCX(t *testing.T, documentXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := w.Write([]byte(documentXML)); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// TestParseDOCX checks the happy path of the Office Open XML extraction.
func TestParseDOCX(t *testing.T) {
	data := buildDOCX(t, `<w:document><w:body>`+
		`<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:tab/><w:t>World</w:t></w:r></w:p>`+
		`</w:body></w:document>`)

	got, err := Extract("test.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") {
		t.Errorf("extracted text = %q", got)
	}
}

// TestParseDOCXRejectsOversizedPart is the regression test for the zip-bomb
// guard: a small archive that expands beyond the limit must be rejected instead
// of being read into memory.
func TestParseDOCXRejectsOversizedPart(t *testing.T) {
	huge := strings.Repeat("a", maxDOCXPartBytes+1024)
	data := buildDOCX(t, `<w:document><w:body><w:p><w:r><w:t>`+huge+`</w:t></w:r></w:p></w:body></w:document>`)

	if _, err := Extract("bomb.docx", "", data); err == nil {
		t.Fatal("expected an error for an oversized document.xml")
	}
}

// TestExtractCapsText makes sure the extracted plain text stays bounded and
// remains valid UTF-8 after truncation.
func TestExtractCapsText(t *testing.T) {
	// Multi-byte runes so a naive byte cut would split a character.
	body := strings.Repeat("ä", maxTextBytes)
	got, err := Extract("big.txt", "text/plain", []byte(body))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) > maxTextBytes {
		t.Errorf("text was not capped: %d bytes", len(got))
	}
	if !isValidUTF8(got) {
		t.Error("truncation produced invalid UTF-8")
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// TestExtractUnsupportedFormat verifies unknown formats are refused.
func TestExtractUnsupportedFormat(t *testing.T) {
	if _, err := Extract("image.png", "image/png", []byte{0x89, 'P', 'N', 'G'}); err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
}
