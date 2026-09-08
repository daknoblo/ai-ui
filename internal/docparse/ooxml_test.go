package docparse

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestOOXMLCumulativeInflatedLimit(t *testing.T) {
	body := `<w:document><w:p><w:t>Body</w:t></w:p></w:document>`
	side := `<w:hdr><w:p><w:t>Side</w:t></w:p></w:hdr>`
	sheet := `<worksheet><row><c><v>42</v></c></row></worksheet>`
	for _, tt := range []struct {
		name  string
		parts map[string]string
		parse func(*ooxmlArchive) (string, error)
	}{
		{"docx", map[string]string{"word/document.xml": body, "word/header1.xml": side}, parseDOCXArchive},
		{"xlsx", map[string]string{"xl/worksheets/sheet1.xml": sheet, "xl/worksheets/sheet2.xml": sheet}, parseXLSXArchive},
		{"pptx", map[string]string{
			"ppt/slides/slide1.xml": drawingPart("One"), "ppt/slides/slide2.xml": drawingPart("Two"),
			"ppt/notesSlides/notesSlide2.xml": drawingPart("Note"),
		}, parsePPTXArchive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limits := defaultOOXMLLimits()
			limits.totalBytes = -1
			for _, raw := range tt.parts {
				limits.totalBytes += int64(len(raw))
			}
			a, err := openOOXMLWithLimits(buildZIP(t, tt.parts), tt.name, limits)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tt.parse(a)
			if !errors.Is(err, errOOXMLLimit) || got != "" {
				t.Fatalf("parse = %q, %v; want explicit total limit failure", got, err)
			}
		})
	}
}

func TestOOXMLMetadataSpendsSharedBudgets(t *testing.T) {
	parts := workbookParts()
	limits := defaultOOXMLLimits()
	limits.totalBytes = int64(len(parts["xl/_rels/workbook.xml.rels"]) + len(parts["xl/workbook.xml"]) - 1)
	a, err := openOOXMLWithLimits(buildZIP(t, parts), "xlsx", limits)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parseXLSXArchive(a); !errors.Is(err, errOOXMLLimit) || got != "" {
		t.Fatalf("ordering metadata ignored inflated budget: %q, %v", got, err)
	}

	a, err = openOOXML(buildZIP(t, parts), "xlsx")
	if err != nil {
		t.Fatal(err)
	}
	a.budget.limits.tokens = 3
	if got, err := parseXLSXArchive(a); !errors.Is(err, errOOXMLLimit) || got != "" {
		t.Fatalf("relationships ignored token budget: %q, %v", got, err)
	}
}

func TestOOXMLArchiveAndReadLimits(t *testing.T) {
	parts := map[string]string{"word/document.xml": "1234", "word/header1.xml": "5678"}
	data := buildZIP(t, parts)
	limits := defaultOOXMLLimits()
	limits.parts = 1
	if _, err := openOOXMLWithLimits(data, "docx", limits); !errors.Is(err, errOOXMLLimit) {
		t.Fatalf("archive count limit = %v", err)
	}
	a, err := openOOXML(data, "docx")
	if err != nil {
		t.Fatal(err)
	}
	a.budget.limits.parts = 2
	a.budget.limits.partBytes, a.budget.limits.totalBytes, a.budget.limits.workBytes = 4, 8, 8
	for range 2 {
		if raw, err := a.readPart(findPart(a, "word/document.xml")); err != nil || string(raw) != "1234" {
			t.Fatalf("read at exact limit = %q, %v", raw, err)
		}
	}
	if raw, err := a.readPart(findPart(a, "word/document.xml")); !errors.Is(err, errOOXMLLimit) || raw != nil {
		t.Fatalf("repeated read bypassed budget: %q, %v", raw, err)
	}
}

func TestOOXMLPartLimitsCheckDeclaredAndActualSize(t *testing.T) {
	data := buildZIP(t, map[string]string{"word/document.xml": strings.Repeat("x", 256)})
	for _, understated := range []bool{false, true} {
		limits := defaultOOXMLLimits()
		limits.partBytes = 128
		a, err := openOOXMLWithLimits(data, "docx", limits)
		if err != nil {
			t.Fatal(err)
		}
		f := findPart(a, "word/document.xml")
		if understated {
			// A lying header must not bypass the actual bounded read. The ZIP
			// checksum reader may reject the size before the budget does.
			f.UncompressedSize64 = 1
		}
		if raw, err := a.readPart(f); err == nil || raw != nil {
			t.Fatalf("readPart(understated=%v) = %d bytes, %v", understated, len(raw), err)
		}
	}
}

func TestOOXMLTextAndWorkLimits(t *testing.T) {
	for _, tt := range []struct {
		name  string
		raw   string
		setup func(*ooxmlLimits)
		parse func([]byte, *ooxmlBudget) (string, error)
	}{
		{"DOCX buffered text", `<w:document><w:tc><w:t>123456789</w:t></w:tc></w:document>`,
			func(l *ooxmlLimits) { l.textBytes = 8 }, extractDOCXText},
		{"DOCX nested copy work", `<w:document><w:tc><w:tc><w:t>12345678</w:t></w:tc></w:tc></w:document>`,
			func(l *ooxmlLimits) { l.workBytes = 16 }, extractDOCXText},
		{"Drawing text", drawingPart("123456789"),
			func(l *ooxmlLimits) { l.textBytes = 8 }, extractDrawingText},
		{"Drawing empty markup", `<p:sld><a:p/><a:p/><a:p/><a:p/></p:sld>`,
			func(l *ooxmlLimits) { l.tokens = 5 }, extractDrawingText},
		{"Drawing depth", `<p:sld><a:p><a:r><a:t>X</a:t></a:r></a:p></p:sld>`,
			func(l *ooxmlLimits) { l.depth = 3 }, extractDrawingText},
	} {
		t.Run(tt.name, func(t *testing.T) {
			budget := testOOXMLBudget()
			tt.setup(&budget.limits)
			got, err := tt.parse([]byte(tt.raw), budget)
			if !errors.Is(err, errOOXMLLimit) || got != "" {
				t.Fatalf("parse = %q, %v; want an explicit limit error", got, err)
			}
		})
	}
	budget := testOOXMLBudget()
	budget.limits.textBytes = 8
	if got, err := extractSharedStrings([]byte(`<sst><si><t>12345</t></si><si><t>67890</t></si></sst>`), budget); !errors.Is(err, errOOXMLLimit) || got != nil {
		t.Fatalf("shared string total unbounded: %v, %v", got, err)
	}
	for _, limit := range []string{"text", "work", "tokens", "depth"} {
		budget := testOOXMLBudget()
		switch limit {
		case "text":
			budget.limits.textBytes = 10
		case "work":
			budget.limits.workBytes = 10
		case "tokens":
			budget.limits.tokens = 4
		case "depth":
			budget.limits.depth = 2
		}
		got, err := extractSheet([]byte(`<worksheet><row><c r="XFD1"><v>42</v></c></row></worksheet>`), nil, budget)
		if !errors.Is(err, errOOXMLLimit) || got != "" {
			t.Errorf("sheet %s limit ignored: %q, %v", limit, got, err)
		}
	}
}

func TestOOXMLDocumentOutputLimitIncludesEveryPart(t *testing.T) {
	for _, tt := range []struct {
		name  string
		parts map[string]string
		parse func(*ooxmlArchive) (string, error)
	}{
		{"docx", map[string]string{
			"word/document.xml": `<w:document><w:t>12345678</w:t></w:document>`,
			"word/header1.xml":  `<w:hdr><w:t>abcdefgh</w:t></w:hdr>`,
		}, parseDOCXArchive},
		{"xlsx", map[string]string{
			"xl/worksheets/sheet1.xml": `<worksheet><row><c><v>12345678</v></c></row></worksheet>`,
			"xl/worksheets/sheet2.xml": `<worksheet><row><c><v>abcdefgh</v></c></row></worksheet>`,
		}, parseXLSXArchive},
		{"pptx", map[string]string{
			"ppt/slides/slide1.xml": drawingPart("12345678"), "ppt/slides/slide2.xml": drawingPart("abcdefgh"),
		}, parsePPTXArchive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := openOOXML(buildZIP(t, tt.parts), tt.name)
			if err != nil {
				t.Fatal(err)
			}
			a.budget.limits.textBytes = 24
			if got, err := tt.parse(a); !errors.Is(err, errOOXMLLimit) || got != "" {
				t.Fatalf("aggregate output was silently truncated: %q, %v", got, err)
			}
		})
	}
}

func TestOOXMLInvalidRelationshipsFailWithoutFallback(t *testing.T) {
	for _, tt := range []struct{ name, old, replacement string }{
		{"external", `Target="/xl/worksheets/custom.xml"`, `TargetMode="External" Target="https://example.com/sheet.xml"`},
		{"network", `Target="/xl/worksheets/custom.xml"`, `Target="//example.com/sheet.xml"`},
		{"traversal", `Target="/xl/worksheets/custom.xml"`, `Target="../../outside.xml"`},
		{"encoded traversal", `Target="/xl/worksheets/custom.xml"`, `Target="%2e%2e/%2e%2e/outside.xml"`},
		{"missing target", `Target="/xl/worksheets/custom.xml"`, `Target="worksheets/missing.xml"`},
		{"wrong type", `/worksheet"`, `/hyperlink"`},
		{"duplicate id", `Id="rSecond"`, `Id="rFirst"`},
		{"malformed XML", `</Relationships>`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parts := workbookParts()
			parts["xl/_rels/workbook.xml.rels"] = strings.ReplaceAll(parts["xl/_rels/workbook.xml.rels"], tt.old, tt.replacement)
			got, err := Extract("invalid.xlsx", "", buildZIP(t, parts))
			if err == nil || got != "" {
				t.Fatalf("invalid relationship silently accepted: %q, %v", got, err)
			}
		})
	}
	for _, missing := range []string{"xl/_rels/workbook.xml.rels", "xl/workbook.xml"} {
		parts := workbookParts()
		delete(parts, missing)
		if got, err := Extract("missing.xlsx", "", buildZIP(t, parts)); err == nil || got != "" {
			t.Fatalf("missing %s silently fell back: %q, %v", missing, got, err)
		}
	}
	for _, missing := range []string{"ppt/_rels/presentation.xml.rels", "ppt/notesSlides/notesSlide7.xml"} {
		parts := presentationParts()
		delete(parts, missing)
		if got, err := Extract("missing.pptx", "", buildZIP(t, parts)); err == nil || got != "" {
			t.Fatalf("missing %s silently fell back: %q, %v", missing, got, err)
		}
	}
	parts := presentationParts()
	parts["ppt/slides/_rels/slide2.xml.rels"] = `<Relationships><Relationship Id="notes" Type="` +
		officeRelationships + `/notesSlide" TargetMode="External" Target="https://example.com/notes.xml"/></Relationships>`
	if got, err := Extract("external-notes.pptx", "", buildZIP(t, parts)); err == nil || got != "" {
		t.Fatalf("external notes relationship accepted: %q, %v", got, err)
	}
}

func TestOOXMLRelationshipTargetsStayInPackage(t *testing.T) {
	for _, target := range []string{
		"../../../outside.xml", "/../outside.xml", "https://example.com/x", "//example.com/x",
		`..\notes.xml`, "%2e%2e/%2e%2e/%2e%2e/outside.xml", "%2f%2fhost/x", "file:///x",
		"slide.xml?x=1", "slide.xml#fragment", "", "%zz", "C:/file.xml", "x%00.xml",
	} {
		if got, err := relationshipTarget("ppt/slides/slide1.xml", target); err == nil {
			t.Errorf("target %q accepted as %q", target, got)
		}
	}
	for target, want := range map[string]string{
		"../notesSlides/note.xml": "ppt/notesSlides/note.xml",
		"/ppt/slides/slide2.xml":  "ppt/slides/slide2.xml",
		"./slide2.xml":            "ppt/slides/slide2.xml",
	} {
		if got, err := relationshipTarget("ppt/slides/slide1.xml", target); err != nil || got != want {
			t.Errorf("target %q = %q, %v; want %q", target, got, err, want)
		}
	}
}

func TestOOXMLRejectsUnsafeAndDuplicateArchiveNames(t *testing.T) {
	for _, name := range []string{"../outside.xml", "/word/document.xml", `word\document.xml`, "word/../outside.xml", "word//document.xml"} {
		if _, err := openOOXML(buildZIP(t, map[string]string{name: "text"}), "docx"); err == nil {
			t.Errorf("unsafe archive name %q accepted", name)
		}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for range 2 {
		if _, err := zw.Create("word/document.xml"); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openOOXML(buf.Bytes(), "docx"); err == nil {
		t.Fatal("duplicate archive part accepted")
	}
}

func TestOOXMLMalformedSidePartsAreNotSilentlyIgnored(t *testing.T) {
	for _, tt := range []struct {
		name  string
		parts map[string]string
	}{
		{"bad.docx", map[string]string{
			"word/document.xml": `<w:document><w:t>Body</w:t></w:document>`,
			"word/header1.xml":  `<w:hdr><w:t>Missing close`,
		}},
		{"bad.xlsx", map[string]string{
			"xl/worksheets/sheet1.xml": `<worksheet><row><c><v>Good</v></c></row></worksheet>`,
			"xl/worksheets/sheet2.xml": `<worksheet><row>`,
		}},
		{"bad.pptx", map[string]string{
			"ppt/slides/slide1.xml": drawingPart("Good"), "ppt/notesSlides/notesSlide1.xml": `<p:notes>`,
		}},
	} {
		if got, err := Extract(tt.name, "", buildZIP(t, tt.parts)); err == nil || got != "" {
			t.Errorf("malformed %s returned partial success: %q, %v", tt.name, got, err)
		}
	}
}
