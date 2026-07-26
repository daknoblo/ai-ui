package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxDOCXPartBytes caps the uncompressed size of word/document.xml. A .docx is
// a ZIP archive, so a few hundred kilobytes on disk can expand into gigabytes
// in memory ("zip bomb"). Both the declared size and the actual read are
// bounded, because the declared size in the ZIP header cannot be trusted.
const maxDOCXPartBytes = 64 << 20 // 64 MiB

// parseDOCX extracts the text from a .docx file (Office Open XML).
// A .docx is a ZIP archive; the body text lives in word/document.xml.
func parseDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read docx (zip): %w", err)
	}

	var docXML []byte
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		if f.UncompressedSize64 > maxDOCXPartBytes {
			return "", fmt.Errorf("docx document.xml too large (%d bytes)", f.UncompressedSize64)
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		// Read one byte beyond the limit so an understated header is detected.
		docXML, err = io.ReadAll(io.LimitReader(rc, maxDOCXPartBytes+1))
		_ = rc.Close()
		if err != nil {
			return "", err
		}
		if len(docXML) > maxDOCXPartBytes {
			return "", fmt.Errorf("docx document.xml exceeds the size limit")
		}
		break
	}
	if docXML == nil {
		return "", fmt.Errorf("word/document.xml not found in docx")
	}

	text, err := extractDOCXText(docXML)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(text)
	if out == "" {
		return "", fmt.Errorf("no extractable text found in docx")
	}
	return out, nil
}

// extractDOCXText reads the relevant elements from document.xml:
//   - <w:t>   text runs
//   - <w:tab> tabulator
//   - <w:br> / <w:cr> line break
//   - </w:p> end of paragraph
func extractDOCXText(xmlData []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var sb strings.Builder

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
			case "tab":
				sb.WriteString("\t")
			case "br", "cr":
				sb.WriteString("\n")
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				sb.WriteString("\n")
			}
		case xml.CharData:
			sb.Write(t)
		}
	}
	return sb.String(), nil
}
