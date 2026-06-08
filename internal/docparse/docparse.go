// Package docparse extrahiert Klartext aus hochgeladenen Dokumenten.
package docparse

import (
	"fmt"
	"strings"
)

// Extract wählt anhand von Dateiname/MIME den passenden Parser und liefert Klartext.
func Extract(filename, mime string, data []byte) (string, error) {
	switch {
	case isText(filename, mime):
		return parseText(data), nil
	case isPDF(filename, mime):
		return parsePDF(data)
	case isDOCX(filename, mime):
		return parseDOCX(data)
	default:
		return "", fmt.Errorf("nicht unterstütztes format: %s (%s)", filename, mime)
	}
}

func isText(filename, mime string) bool {
	lf := strings.ToLower(filename)
	return strings.HasPrefix(mime, "text/") ||
		strings.HasSuffix(lf, ".txt") ||
		strings.HasSuffix(lf, ".md") ||
		strings.HasSuffix(lf, ".markdown") ||
		mime == "application/json" ||
		strings.HasSuffix(lf, ".json")
}

func isPDF(filename, mime string) bool {
	return mime == "application/pdf" || strings.HasSuffix(strings.ToLower(filename), ".pdf")
}

func isDOCX(filename, mime string) bool {
	lf := strings.ToLower(filename)
	return mime == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		strings.HasSuffix(lf, ".docx")
}
