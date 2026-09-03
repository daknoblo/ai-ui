// Package docparse extracts plain text from uploaded documents.
package docparse

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// maxTextBytes caps the plain text extracted from a single upload. Without such
// a limit a small but highly compressed file could expand into gigabytes of
// text and exhaust memory during chunking and embedding.
const maxTextBytes = 8 << 20 // 8 MiB

// errNoText is returned when a parser understood the container but found no
// readable text in it.
var errNoText = errors.New("no extractable text found")

// Extract picks the parser matching the file name/MIME type and returns plain
// text. Anything that is not a known container falls back to a content sniff,
// so a text based format nobody listed here still works.
func Extract(filename, mime string, data []byte) (string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "", fmt.Errorf("file is empty")
	}

	var (
		text string
		err  error
	)
	switch format(filename, mime, data) {
	case formatText:
		text = parseText(data)
	case formatMarkup:
		text = parseMarkup(data)
	case formatPDF:
		text, err = parsePDF(data)
	case formatDOCX:
		text, err = parseDOCX(data)
	case formatXLSX:
		text, err = parseXLSX(data)
	case formatPPTX:
		text, err = parsePPTX(data)
	case formatRTF:
		text, err = parseRTF(data)
	case formatLegacyOffice:
		return "", fmt.Errorf("legacy Office format; please save it as .docx, .xlsx or .pptx and upload it again")
	default:
		return "", fmt.Errorf("unsupported format (%s)", mime)
	}
	if err != nil {
		return "", err
	}

	text = capText(text)
	if strings.TrimSpace(text) == "" {
		return "", errNoText
	}
	return text, nil
}

// kind enumerates the parsers Extract can dispatch to.
type kind int

const (
	formatUnknown kind = iota
	formatText
	formatMarkup // HTML/XML: tags are stripped before the text is used
	formatPDF
	formatDOCX
	formatXLSX
	formatPPTX
	formatRTF
	formatLegacyOffice
)

// extensions maps a file extension to its parser. Everything textual points at
// formatText; the sniffing fallback covers whatever is missing.
var extensions = map[string]kind{
	// documents
	".pdf": formatPDF, ".docx": formatDOCX, ".xlsx": formatXLSX, ".pptx": formatPPTX,
	".docm": formatDOCX, ".xlsm": formatXLSX, ".pptm": formatPPTX,
	".rtf": formatRTF,
	".doc": formatLegacyOffice, ".xls": formatLegacyOffice, ".ppt": formatLegacyOffice,

	// markup
	".html": formatMarkup, ".htm": formatMarkup, ".xhtml": formatMarkup,
	".xml": formatMarkup, ".svg": formatMarkup,

	// plain text and data
	".txt": formatText, ".md": formatText, ".markdown": formatText, ".rst": formatText,
	".json": formatText, ".jsonl": formatText, ".ndjson": formatText,
	".csv": formatText, ".tsv": formatText, ".log": formatText,
	".yaml": formatText, ".yml": formatText, ".toml": formatText,
	".ini": formatText, ".cfg": formatText, ".conf": formatText, ".env": formatText,
	".properties": formatText, ".tex": formatText, ".srt": formatText, ".vtt": formatText,

	// source code
	".go": formatText, ".py": formatText, ".rb": formatText, ".rs": formatText,
	".js": formatText, ".mjs": formatText, ".cjs": formatText, ".ts": formatText,
	".tsx": formatText, ".jsx": formatText, ".vue": formatText, ".svelte": formatText,
	".java": formatText, ".kt": formatText, ".kts": formatText, ".scala": formatText,
	".c": formatText, ".h": formatText, ".cc": formatText, ".cpp": formatText,
	".hpp": formatText, ".cs": formatText, ".swift": formatText, ".m": formatText,
	".php": formatText, ".pl": formatText, ".lua": formatText, ".r": formatText,
	".sh": formatText, ".bash": formatText, ".zsh": formatText, ".fish": formatText,
	".ps1": formatText, ".bat": formatText, ".cmd": formatText,
	".sql": formatText, ".graphql": formatText, ".proto": formatText,
	".tf": formatText, ".tfvars": formatText, ".hcl": formatText,
	".css": formatText, ".scss": formatText, ".less": formatText,
	".dockerfile": formatText, ".gradle": formatText, ".cmake": formatText,
	".diff": formatText, ".patch": formatText,
}

// mimes maps the content types a browser sends to a parser. The extension wins
// when both are known, because browsers derive the type from the extension
// anyway and get it wrong for many of the entries above.
var mimes = map[string]kind{
	"application/pdf": formatPDF,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   formatDOCX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         formatXLSX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": formatPPTX,
	"application/msword":            formatLegacyOffice,
	"application/vnd.ms-excel":      formatLegacyOffice,
	"application/vnd.ms-powerpoint": formatLegacyOffice,
	"application/rtf":               formatRTF,
	"text/rtf":                      formatRTF,
	"application/json":              formatText,
	"application/x-ndjson":          formatText,
	"application/yaml":              formatText,
	"application/toml":              formatText,
	"application/xml":               formatMarkup,
	"text/xml":                      formatMarkup,
	"text/html":                     formatMarkup,
}

// UploadAccept is the accept attribute of the file picker: the known extensions
// plus the image types the server keeps as attachments for the model. Sniffing
// accepts more than this, but the picker needs concrete entries to filter with.
func UploadAccept() string {
	exts := make([]string, 0, len(extensions)+len(imageExtensions))
	for ext := range extensions {
		if extensions[ext] == formatLegacyOffice {
			continue // listing it would suggest it works
		}
		exts = append(exts, ext)
	}
	exts = append(exts, imageExtensions...)
	slices.Sort(exts)
	return strings.Join(exts, ",")
}

// imageExtensions are kept as attachments for the model instead of being
// parsed, but they belong in the picker.
var imageExtensions = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// format decides which parser handles an upload: extension first, then the
// declared MIME type, then the content itself.
func format(filename, mime string, data []byte) kind {
	if k, ok := extensions[strings.ToLower(filepath.Ext(filename))]; ok {
		return k
	}
	// A file without an extension may still carry a well known name.
	if k, ok := extensions["."+strings.ToLower(filepath.Base(filename))]; ok {
		return k
	}

	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	if k, ok := mimes[mime]; ok {
		return k
	}
	if strings.HasPrefix(mime, "text/") {
		return formatText
	}

	return sniff(data)
}

// sniff identifies an upload by its content. It exists so an attachment without
// a useful name or content type still reaches a parser instead of being
// rejected outright.
func sniff(data []byte) kind {
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return formatPDF
	case bytes.HasPrefix(data, []byte(`{\rtf`)):
		return formatRTF
	// D0 CF 11 E0 is the OLE2 compound file header of the pre-2007 formats.
	case bytes.HasPrefix(data, []byte{0xD0, 0xCF, 0x11, 0xE0}):
		return formatLegacyOffice
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return sniffZIP(data)
	case looksLikeMarkup(data):
		return formatMarkup
	case looksLikeText(data):
		return formatText
	}
	return formatUnknown
}

// sniffZIP tells the OOXML formats apart by the parts they contain.
func sniffZIP(data []byte) kind {
	zr, err := openOOXML(data, "archive")
	if err != nil {
		return formatUnknown
	}
	for _, f := range zr.File {
		switch {
		case strings.HasPrefix(f.Name, "word/"):
			return formatDOCX
		case strings.HasPrefix(f.Name, "xl/"):
			return formatXLSX
		case strings.HasPrefix(f.Name, "ppt/"):
			return formatPPTX
		}
	}
	return formatUnknown
}

// sniffBytes is how much of an upload the content heuristics look at.
const sniffBytes = 8 << 10

// looksLikeMarkup reports whether the content starts like an HTML or XML file.
func looksLikeMarkup(data []byte) bool {
	head := bytes.ToLower(bytes.TrimSpace(data[:min(len(data), sniffBytes)]))
	return bytes.HasPrefix(head, []byte("<?xml")) ||
		bytes.HasPrefix(head, []byte("<!doctype html")) ||
		bytes.HasPrefix(head, []byte("<html"))
}

// looksLikeText reports whether the content is valid UTF-8 without the control
// characters that mark a binary file. That is what makes an unknown but textual
// attachment usable instead of rejected.
func looksLikeText(data []byte) bool {
	head := data[:min(len(data), sniffBytes)]
	// A multi-byte rune cut in half at the sniff boundary is not a real error.
	if !utf8.Valid(head) && !utf8.Valid(trimPartialRune(head)) {
		return false
	}
	for _, b := range head {
		// Allow tab, newline, carriage return and form feed; reject the rest of
		// the C0 range, which no text format uses.
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' {
			return false
		}
	}
	return true
}

// trimPartialRune drops an incomplete rune at the end of a sniffed prefix.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax-1; i-- {
		if utf8.RuneStart(b[i]) {
			return b[:i]
		}
	}
	return b
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
