// Package web bundles the embedded templates and static assets.
package web

import "embed"

// TemplatesFS contains all HTML templates.
//
//go:embed templates/*.html
var TemplatesFS embed.FS

// StaticFS contains CSS and JavaScript.
//
//go:embed static/*
var StaticFS embed.FS
