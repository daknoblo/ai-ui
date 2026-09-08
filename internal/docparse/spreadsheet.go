package docparse

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// parseXLSX extracts the cell values of a workbook as tab separated rows.
// A sheet is a table, so the rows survive as rows: that is what makes a
// question like "what is the figure in the revenue column?" answerable.
func parseXLSX(data []byte) (string, error) {
	zr, err := openOOXML(data, "xlsx")
	if err != nil {
		return "", err
	}
	return parseXLSXArchive(zr)
}

func parseXLSXArchive(zr *ooxmlArchive) (string, error) {
	// Most strings live in a single shared table; the cells reference them by
	// index. Without it a workbook reads as a list of numbers.
	var shared []string
	if f := findPart(zr, "xl/sharedStrings.xml"); f != nil {
		raw, err := zr.readPart(f)
		if err != nil {
			return "", err
		}
		if shared, err = extractSharedStrings(raw, zr.budget); err != nil {
			return "", err
		}
	}

	sheets, _, err := zr.orderedParts("xl/workbook.xml", "sheets", "sheet", "worksheet", "xl/worksheets/")
	if err != nil {
		return "", err
	}
	if len(sheets) == 0 {
		return "", errNoText
	}

	sb := ooxmlText{budget: zr.budget}
	for i, sheet := range sheets {
		raw, err := zr.readPart(sheet.file)
		if err != nil {
			return "", err
		}
		text, err := extractSheet(raw, shared, zr.budget)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		name := "Sheet " + strconv.Itoa(i+1)
		if sheet.name != "" {
			name = sheet.name
		}
		sb.write("# " + name + "\n")
		sb.write(strings.TrimRight(text, "\n"))
		sb.write("\n\n")
		if sb.err != nil {
			return "", sb.err
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", errNoText
	}
	return out, nil
}

// extractSharedStrings reads xl/sharedStrings.xml into an indexable slice.
func extractSharedStrings(xmlData []byte, budget *ooxmlBudget) ([]string, error) {
	dec := newOOXMLDecoder(xmlData, budget)
	var (
		out    []string
		cur    = ooxmlText{budget: budget}
		inItem bool
		inText bool
		total  int
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				if inItem {
					return nil, fmt.Errorf("nested XLSX shared string")
				}
				inItem = true
				cur.Reset()
			case "t":
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inItem = false
				if cur.Len() > budget.limits.textBytes-total {
					return nil, fmt.Errorf("%w: shared strings size", errOOXMLLimit)
				}
				total += cur.Len()
				out = append(out, cur.String())
			case "t":
				inText = false
			}
		case xml.CharData:
			// A string can be split across several runs (mixed formatting), so
			// the parts are concatenated instead of replacing each other.
			if inItem && inText {
				cur.write(string(t))
			}
		}
		if cur.err != nil {
			return nil, cur.err
		}
	}
	return out, nil
}

// extractSheet turns a worksheet into tab separated rows.
func extractSheet(xmlData []byte, shared []string, budget *ooxmlBudget) (string, error) {
	dec := newOOXMLDecoder(xmlData, budget)
	var (
		sb       = ooxmlText{budget: budget}
		value    = ooxmlText{budget: budget}
		cellType string
		inValue  bool
		inText   bool
		inRow    bool
		inCell   bool
		lastCol  int
		cellCol  int
		rowIndex int
	)

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
			case "row":
				if inRow {
					return "", fmt.Errorf("nested XLSX row")
				}
				inRow, lastCol, rowIndex = true, 0, 0
				for _, attr := range t.Attr {
					if attr.Name.Local == "r" {
						rowIndex, err = sheetRow(attr.Value)
						if err != nil {
							return "", err
						}
					}
				}
			case "c":
				if !inRow || inCell {
					return "", fmt.Errorf("XLSX cell outside a row or nested cell")
				}
				inCell = true
				cellCol = lastCol + 1
				cellType = ""
				value.Reset()
				for _, attr := range t.Attr {
					switch attr.Name.Local {
					case "t":
						cellType = attr.Value
					case "r":
						var row int
						cellCol, row, err = sheetCoordinate(attr.Value)
						if err != nil {
							return "", err
						}
						if rowIndex == 0 {
							rowIndex = row
						} else if row != rowIndex {
							return "", fmt.Errorf("inconsistent XLSX cell row")
						}
					}
				}
				if cellCol <= lastCol || cellCol > maxSheetColumns {
					return "", fmt.Errorf("invalid XLSX cell column order or range")
				}
			case "v":
				inValue = inCell
			case "t":
				inText = inCell // inline string
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inValue = false
			case "t":
				inText = false
			case "c":
				gaps := cellCol - lastCol
				if lastCol == 0 {
					gaps--
				}
				sb.write(strings.Repeat("\t", gaps))
				sb.write(resolveCell(cellType, value.String(), shared))
				lastCol = cellCol
				inCell = false
			case "row":
				sb.write("\n")
				inRow = false
			}
		case xml.CharData:
			if inValue || inText {
				value.write(string(t))
			}
		}
		if sb.err != nil {
			return "", sb.err
		}
		if value.err != nil {
			return "", value.err
		}
	}
	return sb.String(), nil
}

const (
	maxSheetColumns = 16384
	maxSheetRows    = 1048576
)

func sheetRow(raw string) (int, error) {
	if len(raw) == 0 || len(raw) > 7 || raw[0] < '1' || raw[0] > '9' {
		return 0, fmt.Errorf("invalid XLSX row coordinate %q", raw)
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || n > maxSheetRows {
		return 0, fmt.Errorf("invalid XLSX row coordinate %q", raw)
	}
	return int(n), nil
}

func sheetCoordinate(raw string) (int, int, error) {
	if len(raw) < 2 || len(raw) > 10 {
		return 0, 0, fmt.Errorf("invalid XLSX cell coordinate")
	}
	col, i := 0, 0
	for i < len(raw) && raw[i] >= 'A' && raw[i] <= 'Z' {
		col = col*26 + int(raw[i]-'A'+1)
		i++
		if col > maxSheetColumns {
			return 0, 0, fmt.Errorf("XLSX column exceeds XFD")
		}
	}
	row, err := sheetRow(raw[i:])
	if err != nil || col == 0 {
		return 0, 0, fmt.Errorf("invalid XLSX cell coordinate %q", raw)
	}
	return col, row, nil
}

// resolveCell turns a raw cell value into text, following the shared string
// table for type "s".
func resolveCell(cellType, raw string, shared []string) string {
	if cellType != "s" {
		return raw
	}
	idx, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || idx < 0 || idx >= len(shared) {
		return ""
	}
	return shared[idx]
}

// parsePPTX extracts the text of every slide, including the speaker notes.
func parsePPTX(data []byte) (string, error) {
	zr, err := openOOXML(data, "pptx")
	if err != nil {
		return "", err
	}
	return parsePPTXArchive(zr)
}

func parsePPTXArchive(zr *ooxmlArchive) (string, error) {
	slides, legacy, err := zr.orderedParts("ppt/presentation.xml", "sldIdLst", "sldId", "slide", "ppt/slides/slide")
	if err != nil {
		return "", err
	}
	if len(slides) == 0 {
		return "", errNoText
	}

	sb := ooxmlText{budget: zr.budget}
	for i, slide := range slides {
		raw, err := zr.readPart(slide.file)
		if err != nil {
			return "", err
		}
		text, err := extractDrawingText(raw, zr.budget)
		if err != nil {
			return "", err
		}
		sb.write("# Slide " + strconv.Itoa(i+1) + "\n")
		sb.write(strings.TrimSpace(text))
		sb.write("\n")

		notes, err := slideNotes(zr, slide.file, legacy)
		if err != nil {
			return "", err
		}
		if notes != nil {
			rawNotes, err := zr.readPart(notes)
			if err != nil {
				return "", err
			}
			note, err := extractDrawingText(rawNotes, zr.budget)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(note) != "" {
				sb.write("[notes] " + strings.TrimSpace(note) + "\n")
			}
		}
		sb.write("\n")
		if sb.err != nil {
			return "", sb.err
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", errNoText
	}
	return out, nil
}

func slideNotes(zr *ooxmlArchive, slide *zip.File, legacy bool) (*zip.File, error) {
	rels, hasRels, err := zr.relationships(slide.Name)
	if err != nil {
		return nil, err
	}
	var notes *zip.File
	for _, rel := range rels {
		if rel.kind != "notesSlide" {
			continue
		}
		if notes != nil {
			return nil, fmt.Errorf("multiple notes parts for slide %q", slide.Name)
		}
		if notes, err = zr.relatedPart(rel, "notesSlide"); err != nil {
			return nil, err
		}
	}
	if legacy && !hasRels {
		suffix := strings.TrimPrefix(path.Base(slide.Name), "slide")
		notes = findPart(zr, "ppt/notesSlides/notesSlide"+suffix)
	}
	return notes, nil
}

// extractDrawingText reads the <a:t> runs of a DrawingML part, which is how
// PowerPoint stores the text of every shape. A paragraph ends a line.
func extractDrawingText(xmlData []byte, budget *ooxmlBudget) (string, error) {
	dec := newOOXMLDecoder(xmlData, budget)
	var (
		sb     = ooxmlText{budget: budget}
		inText bool
	)
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
			case "br":
				sb.write("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.write("\n")
			}
		case xml.CharData:
			if inText {
				sb.write(string(t))
			}
		}
		if sb.err != nil {
			return "", sb.err
		}
	}
	return sb.String(), nil
}
