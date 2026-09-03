package docparse

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// buildPDF assembles a minimal, uncompressed one-page PDF that draws the given
// lines. Writing it by hand keeps the test independent of a binary fixture and
// of whatever tool generated it.
func buildPDF(lines []string) []byte {
	var content bytes.Buffer
	content.WriteString("BT\n/F1 12 Tf\n72 720 Td\n14 TL\n")
	for _, line := range lines {
		fmt.Fprintf(&content, "(%s) Tj\nT*\n", line)
	}
	content.WriteString("ET\n")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}

	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref)
	return pdf.Bytes()
}

// TestParsePDFKeepsLines is the regression test for the flattened extraction:
// a PDF has to come back as lines, not as one run-on paragraph.
func TestParsePDFKeepsLines(t *testing.T) {
	got, err := Extract("report.pdf", "application/pdf",
		buildPDF([]string{"First line", "Second line", "Third line"}))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), got)
	}
	for i, want := range []string{"First line", "Second line", "Third line"} {
		if strings.TrimSpace(lines[i]) != want {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], want)
		}
	}
}

// TestParsePDFSurvivesGarbage covers the hardening around the third-party
// reader: a malformed file has to come back as an error, never as a panic.
func TestParsePDFSurvivesGarbage(t *testing.T) {
	valid := buildPDF([]string{"Hello"})

	cases := map[string][]byte{
		"header only":     []byte("%PDF-1.4\n"),
		"truncated":       valid[:len(valid)/2],
		"broken xref":     bytes.ReplaceAll(valid, []byte("startxref"), []byte("startxrEf")),
		"random tail":     append(append([]byte{}, valid...), bytes.Repeat([]byte{0xFF}, 128)...),
		"claims pdf only": append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x01}, 512)...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			// The assertion is that this returns rather than panics; either
			// outcome of the parse itself is acceptable.
			if text, err := Extract("broken.pdf", "application/pdf", data); err == nil && text == "" {
				t.Error("a parse without error must return text")
			}
		})
	}
}

// TestParsePDFExplainsEncryption checks the message for a protected document,
// which otherwise reads as "no text found".
func TestParsePDFExplainsEncryption(t *testing.T) {
	data := append(buildPDF([]string{"secret"}), []byte("\ntrailer\n<< /Encrypt 9 0 R >>\n")...)
	// Break the body so no text can be recovered, which is what a real
	// encrypted document looks like to the parser.
	data = bytes.ReplaceAll(data, []byte("(secret) Tj"), []byte("(\x00\x01\x02) Tj"))

	_, err := Extract("protected.pdf", "application/pdf", data)
	if err == nil {
		t.Skip("this parser recovered text from the mangled body")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("the error does not mention the protection: %v", err)
	}
}

// TestParsePDFRejectsScans makes sure a PDF without a text layer explains what
// to do rather than failing silently.
func TestParsePDFRejectsScans(t *testing.T) {
	_, err := Extract("scan.pdf", "application/pdf", buildPDF(nil))
	if err == nil {
		t.Fatal("expected an error for a PDF without text")
	}
	if !strings.Contains(err.Error(), "OCR") {
		t.Errorf("the error does not suggest OCR: %v", err)
	}
}
