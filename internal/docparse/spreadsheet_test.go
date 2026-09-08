package docparse

import (
	"strings"
	"testing"
)

func testOOXMLBudget() *ooxmlBudget {
	return &ooxmlBudget{limits: defaultOOXMLLimits()}
}

func TestXLSXPreservesMissingColumns(t *testing.T) {
	xml := `<worksheet><sheetData>` +
		`<row r="1"><c r="C1" t="inlineStr"><is><t>Leading</t></is></c></row>` +
		`<row r="2"><c r="A2"><v>10</v></c><c r="C2"><v>30</v></c></row>` +
		`<row r="3"><c r="A3"/><c r="D3" t="s"><v>0</v></c></row>` +
		`</sheetData></worksheet>`
	want := "\t\tLeading\n10\t\t30\n\t\t\tShared\n"
	got, err := extractSheet([]byte(xml), []string{"Shared"}, testOOXMLBudget())
	if err != nil || got != want {
		t.Fatalf("extractSheet = %q, %v; want %q", got, err, want)
	}
	data := buildZIP(t, map[string]string{
		"xl/worksheets/sheet1.xml": xml,
		"xl/sharedStrings.xml":     `<sst><si><t>Shared</t></si></sst>`,
	})
	got, err = Extract("sparse.xlsx", "", data)
	if err != nil || got != "# Sheet 1\n"+strings.TrimRight(want, "\n") {
		t.Fatalf("Extract lost leading or intermediate columns: %q, %v", got, err)
	}
}

func TestXLSXCoordinateBoundaries(t *testing.T) {
	for _, ref := range []string{
		"XFE1", "AAAA1", "A0", "A1048577", "A-1", "A+1", "A01", "a1", "1", "A", "$A$1",
		"A99999999999999999999999", strings.Repeat("Z", 50),
	} {
		t.Run(ref, func(t *testing.T) {
			if _, _, err := sheetCoordinate(ref); err == nil {
				t.Errorf("accepted invalid coordinate %q", ref)
			}
		})
	}
	col, row, err := sheetCoordinate("XFD1048576")
	if err != nil || col != maxSheetColumns || row != maxSheetRows {
		t.Fatalf("last coordinate = %d, %d, %v", col, row, err)
	}
	got, err := extractSheet([]byte(`<worksheet><row><c r="XFD1"><v>last</v></c></row></worksheet>`), nil, testOOXMLBudget())
	if err != nil || got != strings.Repeat("\t", maxSheetColumns-1)+"last\n" {
		t.Fatalf("maximum column was not preserved: length=%d, error=%v", len(got), err)
	}
	for _, body := range []string{
		`<row><c r="A1"/><c r="A1"/></row>`,
		`<row><c r="C1"/><c r="B1"/></row>`,
		`<row r="1"><c r="A2"/></row>`,
		`<row><c r="A1"/><c r="C2"/></row>`,
		`<row r="999999999999"><c/></row>`,
		`<row><c r="XFD1"/><c/></row>`,
		`<c><v>orphan</v></c>`, `<row><row/></row>`, `<row><c><c/></c></row>`,
	} {
		if got, err := extractSheet([]byte("<worksheet>"+body+"</worksheet>"), nil, testOOXMLBudget()); err == nil || got != "" {
			t.Errorf("accepted malformed worksheet %q: %q, %v", body, got, err)
		}
	}
}

func workbookParts() map[string]string {
	return map[string]string{
		"xl/workbook.xml": `<workbook xmlns:r="` + officeRelationships + `"><sheets>` +
			`<sheet name="Second tab first" r:id="rSecond"/><sheet name="First tab second" r:id="rFirst"/>` +
			`</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships>` +
			`<Relationship Id="rFirst" Type="` + officeRelationships + `/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rSecond" Type="` + officeRelationships + `/worksheet" Target="/xl/worksheets/custom.xml"/>` +
			`</Relationships>`,
		"xl/worksheets/sheet1.xml": `<worksheet><row><c><v>111</v></c></row></worksheet>`,
		"xl/worksheets/custom.xml": `<worksheet><row><c><v>222</v></c></row></worksheet>`,
		"xl/worksheets/sheet9.xml": `<worksheet><row><c><v>orphan</v></c></row></worksheet>`,
	}
}

func TestXLSXUsesWorkbookRelationships(t *testing.T) {
	for _, strict := range []bool{false, true} {
		parts := workbookParts()
		if strict {
			for name, raw := range parts {
				parts[name] = strings.ReplaceAll(raw, officeRelationships, strictRelationships)
			}
		}
		got, err := Extract("reordered.xlsx", "", buildZIP(t, parts))
		want := "# Second tab first\n222\n\n# First tab second\n111"
		if err != nil || got != want {
			t.Fatalf("Extract(strict=%v) = %q, %v; want %q", strict, got, err, want)
		}
	}
}

func drawingPart(text string) string {
	return `<p:sld><a:p><a:r><a:t>` + text + `</a:t></a:r></a:p></p:sld>`
}

func presentationParts() map[string]string {
	return map[string]string{
		"ppt/presentation.xml": `<p:presentation xmlns:r="` + officeRelationships + `"><p:sldIdLst>` +
			`<p:sldId id="257" r:id="second"/><p:sldId id="256" r:id="first"/>` +
			`</p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships>` +
			`<Relationship Id="first" Type="` + officeRelationships + `/slide" Target="slides/slide1.xml"/>` +
			`<Relationship Id="second" Type="` + officeRelationships + `/slide" Target="slides/slide2.xml"/>` +
			`</Relationships>`,
		"ppt/slides/slide1.xml": drawingPart("First file"),
		"ppt/slides/slide2.xml": drawingPart("Second file"),
		"ppt/slides/slide9.xml": drawingPart("Orphan slide"),
		"ppt/slides/_rels/slide2.xml.rels": `<Relationships>` +
			`<Relationship Id="notes" Type="` + officeRelationships + `/notesSlide" Target="../notesSlides/notesSlide7.xml"/>` +
			`<Relationship Id="link" Type="` + officeRelationships + `/hyperlink" TargetMode="External" Target="https://example.com/"/>` +
			`</Relationships>`,
		"ppt/notesSlides/notesSlide7.xml": drawingPart("Only second has notes"),
		"ppt/notesSlides/notesSlide1.xml": drawingPart("Orphan notes"),
	}
}

func TestPPTXUsesPresentationAndNotesRelationships(t *testing.T) {
	for _, reordered := range []bool{false, true} {
		parts := presentationParts()
		want := "# Slide 1\nSecond file\n[notes] Only second has notes\n\n# Slide 2\nFirst file"
		if !reordered {
			parts["ppt/presentation.xml"] = strings.ReplaceAll(parts["ppt/presentation.xml"],
				`<p:sldId id="257" r:id="second"/><p:sldId id="256" r:id="first"/>`,
				`<p:sldId id="256" r:id="first"/><p:sldId id="257" r:id="second"/>`)
			want = "# Slide 1\nFirst file\n\n# Slide 2\nSecond file\n[notes] Only second has notes"
		}
		got, err := Extract("reordered.pptx", "", buildZIP(t, parts))
		if err != nil || got != want {
			t.Fatalf("Extract(reordered=%v) = %q, %v; want %q", reordered, got, err, want)
		}
	}
}

func TestPPTXMinimalNotesMatchSlideNumber(t *testing.T) {
	parts := map[string]string{
		"ppt/slides/slide1.xml":           drawingPart("First"),
		"ppt/slides/slide2.xml":           drawingPart("Second"),
		"ppt/notesSlides/notesSlide2.xml": drawingPart("Second notes"),
	}
	got, err := Extract("minimal.pptx", "", buildZIP(t, parts))
	want := "# Slide 1\nFirst\n\n# Slide 2\nSecond\n[notes] Second notes"
	if err != nil || got != want {
		t.Fatalf("Extract = %q, %v; want %q", got, err, want)
	}
}
