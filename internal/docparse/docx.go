package docparse

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// docxExtraParts are the parts appended after the body, collected by prefix and
// each of them numbered (header1.xml, footnotes.xml …). A letterhead or a
// footnote often holds exactly the detail a question is about.
var docxExtraParts = []struct{ prefix, label string }{
	{"word/header", "header"},
	{"word/footer", "footer"},
	{"word/footnotes", "footnote"},
	{"word/endnotes", "endnote"},
}

// parseDOCX extracts the text from a .docx file (Office Open XML).
// A .docx is a ZIP archive; the body text lives in word/document.xml.
func parseDOCX(data []byte) (string, error) {
	zr, err := openOOXML(data, "docx")
	if err != nil {
		return "", err
	}

	body := findPart(zr, "word/document.xml")
	if body == nil {
		return "", fmt.Errorf("word/document.xml not found in docx")
	}
	part, err := readPart(body)
	if err != nil {
		return "", err
	}
	text, err := extractDOCXText(part)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(text)

	// The surrounding parts follow the body, so the main text keeps the
	// strongest position in the extracted document.
	for _, extra := range docxExtraParts {
		for _, f := range partsWithPrefix(zr, extra.prefix, ".xml") {
			if sb.Len() >= maxTextBytes {
				break
			}
			raw, err := readPart(f)
			if err != nil {
				continue // a broken side part must not fail the whole document
			}
			side, err := extractDOCXText(raw)
			if err != nil || strings.TrimSpace(side) == "" {
				continue
			}
			sb.WriteString("\n[" + extra.label + "] ")
			sb.WriteString(strings.TrimSpace(side))
			sb.WriteString("\n")
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", errNoText
	}
	return out, nil
}

// extractDOCXText reads the body text from a WordprocessingML part.
//
// Only the character data inside <w:t> is text. Taking every CharData instead
// would pull in three kinds of noise: the indentation of a pretty printed part,
// the field instructions of <w:instrText> (e.g. a HYPERLINK target), and the
// content of <w:delText> - which is what a tracked change *removed*, so the
// model would be handed text the author deleted.
func extractDOCXText(xmlData []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var (
		sb     strings.Builder
		row    strings.Builder // buffers a table row so it ends without a stray tab
		cell   strings.Builder // buffers a table cell so a row stays on one line
		inText bool
		inCell bool
		inRow  bool
	)

	// write appends to the innermost open sink.
	write := func(s string) {
		switch {
		case inCell:
			cell.WriteString(s)
		case inRow:
			row.WriteString(s)
		default:
			sb.WriteString(s)
		}
	}

	// The guard has to count the table buffers too. Text inside a table is
	// written to cell/row and only reaches sb when the row closes, so a
	// document that opens cells but never closes a row would otherwise leave
	// sb.Len() at zero and grow the buffers without any bound.
	for sb.Len()+row.Len()+cell.Len() < maxTextBytes {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				write("\t")
			case "br", "cr":
				write("\n")
			case "tr":
				inRow = true
				row.Reset()
			case "tc":
				inCell = true
				cell.Reset()
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				// Inside a cell a paragraph break must not split the row.
				if inCell {
					cell.WriteString(" ")
				} else {
					write("\n")
				}
			case "tc":
				inCell = false
				row.WriteString(strings.TrimSpace(cell.String()))
				row.WriteString("\t")
				// Nested cells close more than once. Without the reset every
				// closing tag would append the same payload again.
				cell.Reset()
			case "tr":
				inRow = false
				sb.WriteString(strings.TrimRight(row.String(), "\t"))
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				write(string(t))
			}
		}
	}
	return sb.String(), nil
}
