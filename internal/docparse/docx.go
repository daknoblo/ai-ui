package docparse

import (
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
	return parseDOCXArchive(zr)
}

func parseDOCXArchive(zr *ooxmlArchive) (string, error) {
	body := findPart(zr, "word/document.xml")
	if body == nil {
		return "", fmt.Errorf("word/document.xml not found in docx")
	}
	part, err := zr.readPart(body)
	if err != nil {
		return "", err
	}
	text, err := extractDOCXText(part, zr.budget)
	if err != nil {
		return "", err
	}

	sb := ooxmlText{budget: zr.budget}
	sb.write(text)
	if sb.err != nil {
		return "", sb.err
	}

	// The surrounding parts follow the body, so the main text keeps the
	// strongest position in the extracted document.
	for _, extra := range docxExtraParts {
		for _, f := range partsWithPrefix(zr, extra.prefix, ".xml") {
			raw, err := zr.readPart(f)
			if err != nil {
				return "", err
			}
			side, err := extractDOCXText(raw, zr.budget)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(side) == "" {
				continue
			}
			sb.write("\n[" + extra.label + "] ")
			sb.write(strings.TrimSpace(side))
			sb.write("\n")
			if sb.err != nil {
				return "", sb.err
			}
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
func extractDOCXText(xmlData []byte, budget *ooxmlBudget) (string, error) {
	dec := newOOXMLDecoder(xmlData, budget)
	type frame struct {
		kind string
		text ooxmlText
	}
	root := &frame{text: ooxmlText{budget: budget}}
	stack := []*frame{root}
	var (
		inText   bool
		cells    int
		buffered int
		writeErr error
	)

	// All open rows and cells count toward the same live-text limit. Moving
	// text to a parent also spends work, so deep nesting cannot amplify CPU.
	write := func(s string) {
		if writeErr != nil {
			return
		}
		if len(s) > budget.limits.textBytes-buffered {
			writeErr = fmt.Errorf("%w: buffered DOCX text", errOOXMLLimit)
			return
		}
		sink := &stack[len(stack)-1].text
		sink.write(s)
		writeErr = sink.err
		buffered += len(s)
	}

	for {
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
			case "tr", "tc":
				stack = append(stack, &frame{kind: t.Name.Local, text: ooxmlText{budget: budget}})
				if t.Name.Local == "tc" {
					cells++
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				// Inside a cell a paragraph break must not split the row.
				if cells > 0 {
					write(" ")
				} else {
					write("\n")
				}
			case "tc", "tr":
				current := stack[len(stack)-1]
				if len(stack) == 1 || current.kind != t.Name.Local {
					return "", fmt.Errorf("invalid DOCX table nesting")
				}
				buffered -= current.text.Len()
				stack[len(stack)-1] = nil
				stack = stack[:len(stack)-1]
				if t.Name.Local == "tc" {
					cells--
					write(strings.TrimSpace(current.text.String()))
					write("\t")
				} else {
					write(strings.TrimRight(current.text.String(), "\t"))
					if cells > 0 {
						write(" ")
					} else {
						write("\n")
					}
				}
			}
		case xml.CharData:
			if inText {
				write(string(t))
			}
		}
		if writeErr != nil {
			return "", writeErr
		}
	}
	return root.text.String(), nil
}
