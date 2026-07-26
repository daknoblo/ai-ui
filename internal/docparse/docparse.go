// Package docparse extracts plain text from uploaded documents.
package docparse

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxTextBytes caps the plain text extracted from a single upload. Without such
// a limit a small but highly compressed file could expand into gigabytes of
// text and exhaust memory during chunking and embedding.
const maxTextBytes = 8 << 20 // 8 MiB

// Extract picks the parser matching the file name/MIME type and returns plain text.
func Extract(filename, mime string, data []byte) (string, error) {
	var (
		text string
		err  error
	)
	switch {
	case isText(filename, mime):
		text = parseText(data)
	case isPDF(filename, mime):
		text, err = parsePDF(data)
	case isDOCX(filename, mime):
		text, err = parseDOCX(data)
	default:
		return "", fmt.Errorf("unsupported format: %s (%s)", filename, mime)
	}
	if err != nil {
		return "", err
	}
	return capText(text), nil
}

// capText truncates text at a rune boundary so the result stays valid UTF-8.
func capText(s string) string {
	if len(s) <= maxTextBytes {
		return s
	}
	cut := maxTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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
