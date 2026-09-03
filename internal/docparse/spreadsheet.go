package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
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

	// Most strings live in a single shared table; the cells reference them by
	// index. Without it a workbook reads as a list of numbers.
	var shared []string
	if f := findPart(zr, "xl/sharedStrings.xml"); f != nil {
		raw, err := readPart(f)
		if err != nil {
			return "", err
		}
		if shared, err = extractSharedStrings(raw); err != nil {
			return "", err
		}
	}

	names := sheetNames(zr)
	sheets := partsWithPrefix(zr, "xl/worksheets/", ".xml")
	if len(sheets) == 0 {
		return "", errNoText
	}

	var sb strings.Builder
	for i, f := range sheets {
		if sb.Len() >= maxTextBytes {
			break
		}
		raw, err := readPart(f)
		if err != nil {
			continue // one unreadable sheet must not fail the workbook
		}
		text, err := extractSheet(raw, shared)
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		name := "Sheet " + strconv.Itoa(i+1)
		if i < len(names) {
			name = names[i]
		}
		sb.WriteString("# " + name + "\n")
		sb.WriteString(strings.TrimRight(text, "\n"))
		sb.WriteString("\n\n")
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", errNoText
	}
	return out, nil
}

// sheetNames reads the sheet names from the workbook part so the extracted text
// can label each table the way the user sees it in Excel.
func sheetNames(zr *zip.Reader) []string {
	f := findPart(zr, "xl/workbook.xml")
	if f == nil {
		return nil
	}
	raw, err := readPart(f)
	if err != nil {
		return nil
	}
	var names []string
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "sheet" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "name" {
				names = append(names, attr.Value)
				break
			}
		}
	}
	return names
}

// extractSharedStrings reads xl/sharedStrings.xml into an indexable slice.
func extractSharedStrings(xmlData []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var (
		out    []string
		cur    strings.Builder
		inItem bool
		inText bool
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
				inItem = true
				cur.Reset()
			case "t":
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inItem = false
				out = append(out, cur.String())
			case "t":
				inText = false
			}
		case xml.CharData:
			// A string can be split across several runs (mixed formatting), so
			// the parts are concatenated instead of replacing each other.
			if inItem && inText {
				cur.Write(t)
			}
		}
	}
	return out, nil
}

// extractSheet turns a worksheet into tab separated rows.
func extractSheet(xmlData []byte, shared []string) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var (
		sb       strings.Builder
		value    strings.Builder
		cellType string
		inValue  bool
		inText   bool
		firstCol = true
	)

	for sb.Len() < maxTextBytes {
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
				firstCol = true
			case "c":
				cellType = ""
				value.Reset()
				for _, attr := range t.Attr {
					if attr.Name.Local == "t" {
						cellType = attr.Value
					}
				}
			case "v":
				inValue = true
			case "t":
				inText = true // inline string
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inValue = false
			case "t":
				inText = false
			case "c":
				if !firstCol {
					sb.WriteString("\t")
				}
				firstCol = false
				sb.WriteString(resolveCell(cellType, value.String(), shared))
			case "row":
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inValue || inText {
				value.Write(t)
			}
		}
	}
	return sb.String(), nil
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

	slides := partsWithPrefix(zr, "ppt/slides/slide", ".xml")
	notes := partsWithPrefix(zr, "ppt/notesSlides/notesSlide", ".xml")
	if len(slides) == 0 {
		return "", errNoText
	}

	var sb strings.Builder
	for i, f := range slides {
		if sb.Len() >= maxTextBytes {
			break
		}
		raw, err := readPart(f)
		if err != nil {
			continue
		}
		text, err := extractDrawingText(raw)
		if err != nil {
			continue
		}
		sb.WriteString("# Slide " + strconv.Itoa(i+1) + "\n")
		sb.WriteString(strings.TrimSpace(text))
		sb.WriteString("\n")

		if i < len(notes) {
			if rawNotes, err := readPart(notes[i]); err == nil {
				if note, err := extractDrawingText(rawNotes); err == nil && strings.TrimSpace(note) != "" {
					sb.WriteString("[notes] " + strings.TrimSpace(note) + "\n")
				}
			}
		}
		sb.WriteString("\n")
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", errNoText
	}
	return out, nil
}

// extractDrawingText reads the <a:t> runs of a DrawingML part, which is how
// PowerPoint stores the text of every shape. A paragraph ends a line.
func extractDrawingText(xmlData []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var (
		sb     strings.Builder
		inText bool
	)
	for sb.Len() < maxTextBytes {
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
				sb.WriteString("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}
	return sb.String(), nil
}
