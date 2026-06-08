// Package web bündelt die eingebetteten Templates und statischen Assets.
package web

import "embed"

// TemplatesFS enthält alle HTML-Templates.
//
//go:embed templates/*.html
var TemplatesFS embed.FS

// StaticFS enthält CSS und JavaScript.
//
//go:embed static/*
var StaticFS embed.FS
