package docparse

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/ledongthuc/pdf"
)

// maxPDFPages caps how many pages are scanned. A crafted PDF can declare a huge
// page count; stopping early keeps memory and CPU bounded.
const maxPDFPages = 5000

// runOfSpaces matches the whitespace GetPlainText emits to reproduce the
// horizontal position of a text run. Those runs carry no information but cost
// embedding tokens and blur the chunk boundaries.
var runOfSpaces = regexp.MustCompile(`[ \t\x{00A0}]{3,}`)

// parsePDF extracts the text from a PDF document.
func parsePDF(data []byte) (text string, err error) {
	// The reader is a third-party parser fed with untrusted input. It indexes
	// into slices while walking the cross-reference table, so a malformed file
	// can panic instead of returning an error. Recovering here turns that into
	// a normal upload failure rather than a request the middleware has to catch.
	defer func() {
		if r := recover(); r != nil {
			text = ""
			err = fmt.Errorf("pdf is damaged or uses an unsupported structure")
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if isEncryptedPDF(data) {
			return "", fmt.Errorf("pdf is password protected")
		}
		return "", fmt.Errorf("read pdf: %w", err)
	}

	var sb strings.Builder
	totalPages := min(r.NumPage(), maxPDFPages)
	for i := 1; i <= totalPages; i++ {
		if sb.Len() >= maxTextBytes {
			break
		}
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		sb.WriteString(pageText(page))
		sb.WriteString("\n")
	}

	out := strings.TrimSpace(collapseBlankLines(sb.String()))
	if out == "" {
		if isEncryptedPDF(data) {
			return "", fmt.Errorf("pdf is password protected")
		}
		// A scan carries its pages as images. Reporting that separately lets
		// the caller transcribe them instead of rejecting the upload.
		if len(ScanImages(data)) > 0 {
			return "", ErrNeedsOCR
		}
		return "", fmt.Errorf("pdf contains no extractable text - it is likely a scan, so export it with OCR or attach it as an image")
	}
	return out, nil
}

// pageText extracts a single page.
//
// Neither API of the reader is complete on its own, and which one works depends
// on how the generator positioned the text:
//
//   - GetTextByRow groups the runs by baseline and orders them top to bottom,
//     left to right. It only tracks the position of the Tm operator, so a page
//     laid out with Td or T* collapses into a single row.
//   - GetPlainText turns T* into a line break but ignores Tm entirely, so a
//     page laid out with Tm comes back as one long line.
//
// The row count tells the two cases apart: more than one row means the page
// used Tm and the grouping is the better rendering, because it also recovers
// the columns. Anything else falls back to the plain text.
func pageText(page pdf.Page) string {
	rows, err := page.GetTextByRow()
	if err == nil && len(rows) > 1 {
		var sb strings.Builder
		for _, row := range rows {
			var line strings.Builder
			for _, run := range row.Content {
				line.WriteString(run.S)
			}
			sb.WriteString(cleanPDFText(line.String()))
			sb.WriteString("\n")
		}
		if strings.TrimSpace(sb.String()) != "" {
			return sb.String()
		}
	}

	plain, err := page.GetPlainText(nil)
	if err != nil {
		return "" // skip individual broken pages
	}
	if strings.TrimSpace(plain) != "" {
		return cleanPDFText(plain)
	}

	// A single row is still better than nothing when the plain text is empty.
	if len(rows) == 1 {
		var line strings.Builder
		for _, run := range rows[0].Content {
			line.WriteString(run.S)
		}
		return cleanPDFText(line.String())
	}
	return ""
}

// cleanPDFText normalizes the positioning whitespace of an extracted page. Long
// runs of spaces separate columns, so they become a tabulator; everything else
// collapses to a single space.
func cleanPDFText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = runOfSpaces.ReplaceAllString(s, "\t")

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

// isEncryptedPDF reports whether the file declares an encryption dictionary.
// Those documents parse but yield no readable text, which deserves a clearer
// message than "no text found".
func isEncryptedPDF(data []byte) bool {
	// The trailer sits at the end of the file; scanning the tail is enough and
	// keeps the check cheap for large uploads.
	tail := data
	if len(tail) > 4<<10 {
		tail = tail[len(tail)-(4<<10):]
	}
	return bytes.Contains(tail, []byte("/Encrypt"))
}
