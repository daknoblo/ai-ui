package docparse

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	odfOffice = "urn:oasis:names:tc:opendocument:xmlns:office:1.0"
	odfText   = "urn:oasis:names:tc:opendocument:xmlns:text:1.0"
	odfTable  = "urn:oasis:names:tc:opendocument:xmlns:table:1.0"
)

// parseODF reads visible text and table values, never macros, external links or
// embedded files. The same ZIP/XML/work budgets as OOXML apply.
func parseODF(data []byte) (string, error) {
	a, err := openOOXML(data, "OpenDocument")
	if err != nil {
		return "", err
	}
	part := findPart(a, "content.xml")
	if part == nil {
		return "", errors.New("OpenDocument has no content.xml")
	}
	raw, err := a.readPart(part)
	if err != nil {
		return "", err
	}
	dec := newOOXMLDecoder(raw, a.budget)
	out := &ooxmlText{budget: a.budget}
	row, cell := &ooxmlText{budget: a.budget}, &ooxmlText{budget: a.budget}
	var body, paragraph, hidden int
	inRow, inCell := false, false
	rowRepeat, cellRepeat := 1, 1
	value := ""
	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read OpenDocument XML: %w", err)
		}
		switch v := token.(type) {
		case xml.StartElement:
			if hidden > 0 {
				hidden++
				continue
			}
			if v.Name.Space == odfText && (v.Name.Local == "tracked-changes" || v.Name.Local == "deletion") ||
				v.Name.Space == odfOffice && (v.Name.Local == "annotation" || v.Name.Local == "binary-data") {
				hidden = 1
				continue
			}
			if v.Name.Space == odfOffice && v.Name.Local == "body" {
				body++
			}
			if body == 0 {
				continue
			}
			switch {
			case v.Name.Space == odfTable && v.Name.Local == "table":
				for _, attr := range v.Attr {
					if attr.Name.Space == odfTable && attr.Name.Local == "name" {
						out.write("\n" + attr.Value + "\n")
					}
				}
			case v.Name.Space == odfTable && v.Name.Local == "table-row":
				if inRow {
					return "", errors.New("nested OpenDocument table rows are not supported")
				}
				inRow = true
				row.Reset()
				rowRepeat, err = odfRepeat(v, odfTable, "number-rows-repeated")
			case v.Name.Space == odfTable && (v.Name.Local == "table-cell" || v.Name.Local == "covered-table-cell"):
				if inCell {
					return "", errors.New("nested OpenDocument cells are not supported")
				}
				inCell = true
				cell.Reset()
				value = ""
				cellRepeat, err = odfRepeat(v, odfTable, "number-columns-repeated")
				for _, attr := range v.Attr {
					if attr.Name.Space == odfOffice && (attr.Name.Local == "value" || attr.Name.Local == "date-value" ||
						attr.Name.Local == "time-value" || attr.Name.Local == "boolean-value" || attr.Name.Local == "string-value") {
						value = attr.Value
					}
				}
			case v.Name.Space == odfText && (v.Name.Local == "p" || v.Name.Local == "h"):
				paragraph++
			case v.Name.Space == odfText && paragraph > 0:
				dst := out
				if inCell {
					dst = cell
				}
				switch v.Name.Local {
				case "s":
					var count int
					count, err = odfRepeat(v, odfText, "c")
					if err == nil && count <= 10000 {
						dst.write(strings.Repeat(" ", count))
					} else if err == nil {
						err = errors.New("OpenDocument repeated space limit exceeded")
					}
				case "tab":
					dst.write("\t")
				case "line-break":
					dst.write("\n")
				}
			}
			if err != nil {
				return "", err
			}
		case xml.CharData:
			if hidden == 0 && body > 0 && paragraph > 0 {
				if inCell {
					cell.write(string(v))
				} else {
					out.write(string(v))
				}
			}
		case xml.EndElement:
			if hidden > 0 {
				hidden--
				continue
			}
			switch {
			case v.Name.Space == odfOffice && v.Name.Local == "body":
				body--
			case v.Name.Space == odfText && (v.Name.Local == "p" || v.Name.Local == "h") && paragraph > 0:
				paragraph--
				if inCell {
					cell.write("\n")
				} else {
					out.write("\n")
				}
			case v.Name.Space == odfTable && (v.Name.Local == "table-cell" || v.Name.Local == "covered-table-cell") && inCell:
				text := strings.TrimSpace(cell.String())
				if text == "" {
					text = value
				}
				if cellRepeat > 10000 {
					return "", errors.New("OpenDocument repeated column limit exceeded")
				}
				for range cellRepeat {
					row.write(text + "\t")
				}
				inCell = false
			case v.Name.Space == odfTable && v.Name.Local == "table-row" && inRow:
				text := strings.TrimRight(row.String(), "\t\n ")
				// ODS producers often encode millions of trailing empty rows.
				if text != "" {
					if rowRepeat > 10000 {
						return "", errors.New("OpenDocument repeated row limit exceeded")
					}
					for range rowRepeat {
						out.write(text + "\n")
					}
				}
				inRow = false
			}
		}
		if err := errors.Join(out.err, row.err, cell.err); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(out.String()), nil
}

func odfRepeat(element xml.StartElement, namespace, name string) (int, error) {
	for _, attr := range element.Attr {
		if attr.Name.Space == namespace && attr.Name.Local == name {
			count, err := strconv.Atoi(attr.Value)
			if err != nil || count < 1 {
				return 0, errors.New("invalid OpenDocument repeat count")
			}
			return count, nil
		}
	}
	return 1, nil
}
