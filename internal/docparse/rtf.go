package docparse

import (
	"strconv"
	"strings"
)

// parseRTF extracts the plain text of an RTF document.
//
// RTF interleaves text with control words (\b, \par …) and groups ({…}). The
// parser keeps the text, turns the control words that carry layout into
// whitespace and skips the groups that hold no body text - a font table or an
// ignorable destination would otherwise flood the extract with markup.
func parseRTF(data []byte) (string, error) {
	s := string(data)
	var (
		sb    strings.Builder
		depth int
		// skipDepth is the group depth the current skipped destination started
		// at; -1 means nothing is being skipped.
		skipDepth = -1
		i         int
	)

	for i < len(s) && sb.Len() < maxTextBytes {
		switch c := s[i]; c {
		case '{':
			depth++
			i++
		case '}':
			if skipDepth >= 0 && depth <= skipDepth {
				skipDepth = -1
			}
			depth--
			i++
		case '\\':
			word, arg, next := readControl(s, i)
			i = next

			// \* marks a destination a reader may ignore. Skipping it covers
			// every private extension without having to know its name.
			if word == "*" {
				skipDepth = depth
				continue
			}
			if skipDepth >= 0 {
				continue
			}

			switch word {
			case "par", "line", "sect", "page", "\n", "\r":
				// A backslash directly before a line break is RTF's hard return,
				// which readControl reports as the newline itself.
				sb.WriteString("\n")
			case "tab", "cell":
				sb.WriteString("\t")
			case "row":
				sb.WriteString("\n")
			case "u":
				// \uN is a Unicode code point followed by a fallback character
				// for readers that cannot render it.
				if n, err := strconv.Atoi(arg); err == nil {
					if n < 0 {
						n += 65536 // code points above 32767 are written negative
					}
					sb.WriteRune(rune(n))
				}
				i = skipUnicodeFallback(s, i)
			case "'":
				// \'hh is a byte in the document code page.
				if n, err := strconv.ParseInt(arg, 16, 32); err == nil {
					sb.WriteRune(cp1252Rune(byte(n)))
				}
			case "\\", "{", "}":
				sb.WriteString(word)
			default:
				if skipDestinations[word] {
					skipDepth = depth
				}
			}
		default:
			// Raw line breaks are formatting of the RTF source, not of the text.
			if skipDepth < 0 && c != '\r' && c != '\n' {
				sb.WriteByte(c)
			}
			i++
		}
	}

	out := strings.TrimSpace(collapseBlankLines(sb.String()))
	if out == "" {
		return "", errNoText
	}
	return out, nil
}

// skipDestinations name the groups whose content is not body text.
var skipDestinations = map[string]bool{
	"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true,
	"listtable": true, "listoverridetable": true, "revtbl": true, "rsidtbl": true,
	"themedata": true, "datastore": true, "generator": true, "filetbl": true,
	"xmlnstbl": true, "latentstyles": true, "pntext": true, "header": true,
	"footer": true, "pict": true, "object": true, "fldinst": true,
}

// readControl parses the control word at s[i] (which is a backslash) and
// returns the word, its numeric argument and the offset after it.
func readControl(s string, i int) (word, arg string, next int) {
	i++ // consume the backslash
	if i >= len(s) {
		return "", "", i
	}

	switch c := s[i]; {
	case c == '\\' || c == '{' || c == '}':
		return string(c), "", i + 1 // escaped literal
	case c == '\'':
		end := min(i+3, len(s))
		return "'", s[i+1 : end], end // hex byte
	case !isAlpha(c):
		return string(c), "", i + 1 // \*, a hard return, …
	}

	start := i
	for i < len(s) && isAlpha(s[i]) {
		i++
	}
	word = s[start:i]

	argStart := i
	if i < len(s) && (s[i] == '-' || isDigit(s[i])) {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		arg = s[argStart:i]
	}
	// A single space after a control word is its delimiter, not text.
	if i < len(s) && s[i] == ' ' {
		i++
	}
	return word, arg, i
}

// skipUnicodeFallback drops the replacement character that follows \uN.
func skipUnicodeFallback(s string, i int) int {
	if i < len(s) && s[i] == '?' {
		return i + 1
	}
	return i
}

// cp1252High maps the 0x80-0x9F range of Windows-1252, which is the only part
// where it differs from Latin-1. Everything else maps to the same code point,
// so a document written on Windows keeps its umlauts and quotation marks.
var cp1252High = [32]rune{
	'\u20AC', '\uFFFD', '\u201A', '\u0192', '\u201E', '\u2026', '\u2020', '\u2021',
	'\u02C6', '\u2030', '\u0160', '\u2039', '\u0152', '\uFFFD', '\u017D', '\uFFFD',
	'\uFFFD', '\u2018', '\u2019', '\u201C', '\u201D', '\u2022', '\u2013', '\u2014',
	'\u02DC', '\u2122', '\u0161', '\u203A', '\u0153', '\uFFFD', '\u017E', '\u0178',
}

// cp1252Rune decodes a single byte of the document code page.
func cp1252Rune(b byte) rune {
	if b >= 0x80 && b <= 0x9F {
		if r := cp1252High[b-0x80]; r != '\uFFFD' {
			return r
		}
		return ' '
	}
	return rune(b)
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// collapseBlankLines reduces runs of empty lines to a single one, which keeps
// the chunking of a converted document meaningful.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
