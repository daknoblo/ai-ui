package docparse

import (
	"bytes"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"strings"
)

// parseText normalizes plain text/Markdown input.
func parseText(data []byte) string {
	s := string(data)
	// Normalize line endings.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

// blockTags end a line when they close, so a stripped page keeps its structure
// instead of collapsing into a single paragraph.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "section": true,
	"article": true, "header": true, "footer": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "blockquote": true,
	"pre": true, "table": true, "ul": true, "ol": true, "dl": true, "dt": true,
	"dd": true, "hr": true, "nav": true, "aside": true, "main": true, "form": true,
}

// invisibleTags hold content that is never shown to a reader. Keeping it would
// feed the model stylesheets and program code instead of the page text.
var invisibleTags = map[string]bool{
	"script": true, "style": true, "head": true, "noscript": true, "template": true,
}

// parseMarkup strips the tags from an HTML or XML document and keeps the text.
//
// The decoder is deliberately lenient: real world HTML is rarely well formed
// XML, so a parse error falls back to a regex-free manual strip rather than
// failing the upload.
func parseMarkup(data []byte) string {
	if text, ok := stripMarkupXML(data); ok {
		return text
	}
	return stripMarkupLoose(data)
}

// stripMarkupXML walks well formed markup with the XML decoder. It reports
// false as soon as the input turns out not to be well formed.
func stripMarkupXML(data []byte) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	var (
		sb     strings.Builder
		hidden int
	)
	for sb.Len() < maxTextBytes {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false
		}

		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if invisibleTags[name] {
				hidden++
			}
			if blockTags[name] {
				sb.WriteString("\n")
			}
		case xml.EndElement:
			name := strings.ToLower(t.Name.Local)
			if invisibleTags[name] && hidden > 0 {
				hidden--
			}
			if blockTags[name] {
				sb.WriteString("\n")
			}
		case xml.CharData:
			if hidden == 0 {
				sb.Write(t)
			}
		}
	}
	return tidyMarkupText(sb.String()), true
}

// stripMarkupLoose removes everything between angle brackets. It is the
// fallback for markup the decoder rejects, which covers most hand written HTML.
func stripMarkupLoose(data []byte) string {
	var (
		sb     strings.Builder
		inTag  bool
		name   strings.Builder
		hidden int
	)
	for i := 0; i < len(data) && sb.Len() < maxTextBytes; i++ {
		switch c := data[i]; {
		case c == '<':
			inTag = true
			name.Reset()
		case c == '>' && inTag:
			inTag = false
			tag := strings.ToLower(strings.TrimSpace(name.String()))
			closing := strings.HasPrefix(tag, "/")
			tag = strings.TrimPrefix(tag, "/")
			if cut := strings.IndexAny(tag, " \t\n/"); cut >= 0 {
				tag = tag[:cut]
			}
			switch {
			case invisibleTags[tag] && closing && hidden > 0:
				hidden--
			case invisibleTags[tag] && !closing:
				hidden++
			case blockTags[tag]:
				sb.WriteString("\n")
			}
		case inTag:
			if name.Len() < 32 {
				name.WriteByte(c)
			}
		case hidden == 0:
			sb.WriteByte(c)
		}
	}
	return tidyMarkupText(html.UnescapeString(sb.String()))
}

// tidyMarkupText normalizes the whitespace a stripped document is left with.
func tidyMarkupText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return strings.TrimSpace(collapseBlankLines(strings.Join(out, "\n")))
}
