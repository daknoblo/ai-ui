package server

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// markdown is the configured goldmark converter.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(
		gmhtml.WithHardWraps(),
		// Deliberately NO WithUnsafe: raw HTML coming from model or document
		// content is escaped to prevent XSS.
	),
)

// renderMarkdown converts Markdown into safe HTML.
func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(src), &buf); err != nil {
		// Fallback: emit the source escaped as plain text.
		// #nosec G203 -- the value was escaped on the previous expression.
		return template.HTML(template.HTMLEscapeString(src))
	}
	// #nosec G203 -- goldmark is configured without WithUnsafe, so raw HTML in
	// the source has already been escaped by the renderer.
	return template.HTML(buf.String())
}
