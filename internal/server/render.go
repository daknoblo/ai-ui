package server

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// markdown ist der konfigurierte Goldmark-Konverter.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(
		gmhtml.WithHardWraps(),
		// Bewusst KEIN WithUnsafe: rohes HTML aus Modell-/Dokumentinhalten
		// wird escaped, um XSS zu vermeiden.
	),
)

// renderMarkdown wandelt Markdown in sicheres HTML um.
func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(src), &buf); err != nil {
		// Fallback: als reiner Text escaped ausgeben.
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}
