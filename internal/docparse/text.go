package docparse

import "strings"

// parseText normalisiert reinen Text/Markdown.
func parseText(data []byte) string {
	s := string(data)
	// Vereinheitliche Zeilenenden.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}
