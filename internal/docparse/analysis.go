package docparse

import (
	"bytes"
	"path/filepath"
	"strings"
)

// UseDocumentAnalysis chooses Layout for every PDF, including mixed text/scan
// documents, and image formats not supported by the chat image attachment API.
func UseDocumentAnalysis(filename, mime string, data []byte) bool {
	return bytes.HasPrefix(data, []byte("%PDF-")) || format(filename, mime, data) == formatPDF || IsAnalysisImage(filename, mime)
}

// IsAnalysisImage leaves PNG/JPEG/WebP/GIF attachments and image editing intact.
func IsAnalysisImage(filename, mime string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".tif", ".tiff", ".bmp", ".heif", ".heic":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/tiff", "image/bmp", "image/heif", "image/heic":
		return true
	}
	return false
}

// PDFPageCount is a best-effort preflight, not a substitute for service-side
// page validation. PDFs the legacy parser cannot read still reach Layout.
func PDFPageCount(data []byte) int {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return 0
	}
	return pageCount(data)
}
