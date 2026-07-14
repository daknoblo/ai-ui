package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// parseDOCX extrahiert den Text aus einer .docx-Datei (Office Open XML).
// Eine .docx ist ein ZIP-Archiv; der Fließtext steht in word/document.xml.
func parseDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx (zip) lesen: %w", err)
	}

	var docXML []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			docXML, err = io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return "", err
			}
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("word/document.xml nicht im docx gefunden")
	}

	text, err := extractDOCXText(docXML)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(text)
	if out == "" {
		return "", fmt.Errorf("kein extrahierbarer text im docx gefunden")
	}
	return out, nil
}

// extractDOCXText liest die relevanten Elemente aus document.xml:
//   - <w:t>   Textläufe
//   - <w:tab> Tabulator
//   - <w:br> / <w:cr> Zeilenumbruch
//   - </w:p> Absatzende
func extractDOCXText(xmlData []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var sb strings.Builder

	for {
		tok, err := dec.Token()
		if err == io.EOF {
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
