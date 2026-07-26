package docparse

import "strings"

// parseText normalizes plain text/Markdown input.
func parseText(data []byte) string {
	s := string(data)
	// Normalize line endings.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}
