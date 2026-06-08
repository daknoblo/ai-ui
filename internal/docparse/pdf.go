package docparse

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// parsePDF extrahiert den Text aus einem PDF-Dokument.
func parsePDF(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("pdf lesen: %w", err)
	}

	var sb strings.Builder
	totalPages := r.NumPage()
	for i := 1; i <= totalPages; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			continue // einzelne fehlerhafte Seiten überspringen
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("kein extrahierbarer text im pdf gefunden")
	}
	return out, nil
}
