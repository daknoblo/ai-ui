package docparse

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// maxPDFPages caps how many pages are scanned. A crafted PDF can declare a huge
// page count; stopping early keeps memory and CPU bounded.
const maxPDFPages = 5000

// parsePDF extracts the text from a PDF document.
func parsePDF(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}

	var sb strings.Builder
	totalPages := r.NumPage()
	if totalPages > maxPDFPages {
		totalPages = maxPDFPages
	}
	for i := 1; i <= totalPages; i++ {
		if sb.Len() >= maxTextBytes {
			break
		}
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			continue // skip individual broken pages
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("no extractable text found in pdf")
	}
	return out, nil
}
