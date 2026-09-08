package docparse

import (
	"errors"
	"strings"
	"testing"
)

// TestDOCXNestedCellsDoNotRepeat is the regression test for a memory
// amplification hidden in the table handling.
//
// Text inside a table is buffered per cell and flushed into the row when the
// cell closes. The buffer was not reset afterwards, so with properly nested
// cells every closing tag appended the *same* payload again - nesting depth D
// with payload P produced D×P bytes. Since a .docx is a ZIP, a small archive
// could expand far past any input limit that way.
func TestDOCXNestedCellsDoNotRepeat(t *testing.T) {
	data := buildDOCX(t, `<w:document><w:body><w:tbl><w:tr>`+
		`<w:tc><w:tc><w:tc><w:p><w:r><w:t>Once</w:t></w:r></w:p></w:tc></w:tc></w:tc>`+
		`</w:tr></w:tbl></w:body></w:document>`)

	got, err := Extract("nested.docx", "", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if n := strings.Count(got, "Once"); n != 1 {
		t.Errorf("the cell text appears %d times, want 1: %q", n, got)
	}
}

// TestDOCXDeepNestingTerminates covers the other half: the extraction loop
// watches the output buffer, but text inside a table sits in the cell and row
// buffers until the row closes. A document that opens cells and never closes a
// row must still finish instead of running until it is killed.
func TestDOCXDeepNestingTerminates(t *testing.T) {
	const depth = 300

	var xml strings.Builder
	xml.WriteString("<w:document><w:body>")
	for range depth {
		xml.WriteString("<w:tc><w:p><w:r><w:t>x</w:t></w:r></w:p>")
	}
	// Deliberately unclosed: no </w:tc>, no </w:tr>, no </w:tbl>.
	xml.WriteString("</w:body></w:document>")
	data := buildDOCX(t, xml.String())

	if got, err := Extract("deep.docx", "", data); !errors.Is(err, errOOXMLLimit) || got != "" {
		t.Fatalf("Extract = %q, %v; want a nesting limit error", got, err)
	}
}

func TestDOCXNestedTablesPreserveOuterText(t *testing.T) {
	cell := func(text string) string { return `<w:tc><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:tc>` }
	data := buildDOCX(t, `<w:document><w:body><w:tbl><w:tr><w:tc>`+
		`<w:p><w:r><w:t>Before</w:t></w:r></w:p>`+
		`<w:tbl><w:tr>`+cell("Inner A")+cell("Inner B")+`</w:tr>`+
		`<w:tr>`+cell("Inner C")+`</w:tr></w:tbl>`+
		`<w:p><w:r><w:t>After</w:t></w:r></w:p></w:tc>`+
		cell("Outer neighbor")+`</w:tr><w:tr>`+cell("Next row")+
		`</w:tr></w:tbl></w:body></w:document>`)
	got, err := Extract("nested.docx", "", data)
	want := "Before Inner A\tInner B Inner C After\tOuter neighbor\nNext row"
	if err != nil || got != want {
		t.Fatalf("Extract = %q, %v; want %q", got, err, want)
	}
}
