package docparse

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// parseRTF extracts the plain text of an RTF document.
//
// RTF interleaves text with control words (\b, \par …) and groups ({…}). The
// parser keeps the text, turns the control words that carry layout into
// whitespace and skips the groups that hold no body text - a font table or an
// ignorable destination would otherwise flood the extract with markup.
func parseRTF(data []byte) (string, error) {
	s := string(data)
	type group struct {
		fallback int
		skip     bool
	}
	var (
		sb      strings.Builder
		state   = group{fallback: 1}
		stack   []group
		pending rune
		i       int
	)
	flushSurrogate := func() {
		if pending != 0 {
			sb.WriteRune(utf8.RuneError)
			pending = 0
		}
	}
	writeUnit := func(unit rune) {
		if pending != 0 && unit >= 0xDC00 && unit <= 0xDFFF {
			sb.WriteRune(utf16.DecodeRune(pending, unit))
			pending = 0
			return
		}
		flushSurrogate()
		switch {
		case unit >= 0xD800 && unit <= 0xDBFF:
			pending = unit
		case unit >= 0xDC00 && unit <= 0xDFFF:
			sb.WriteRune(utf8.RuneError)
		default:
			sb.WriteRune(unit)
		}
	}
	write := func(text string) {
		flushSurrogate()
		sb.WriteString(text)
	}

	for i < len(s) && sb.Len() < maxTextBytes {
		switch c := s[i]; c {
		case '{':
			if len(stack) >= 256 {
				return "", fmt.Errorf("RTF group nesting limit exceeded")
			}
			stack = append(stack, state)
			i++
		case '}':
			if len(stack) == 0 {
				return "", fmt.Errorf("unmatched RTF closing group")
			}
			state = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			i++
		case '\\':
			word, arg, next := readControl(s, i)
			i = next

			// \* marks a destination a reader may ignore. Skipping it covers
			// every private extension without having to know its name.
			if word == "*" {
				state.skip = true
				continue
			}
			// Binary destinations can contain braces and backslashes. Their
			// payload must never be interpreted as RTF group structure.
			if word == "bin" {
				var err error
				if i, err = skipRTFBinary(s, i, arg); err != nil {
					return "", err
				}
				continue
			}
			if state.skip {
				continue
			}

			switch word {
			case "par", "line", "sect", "page", "\n", "\r":
				// A backslash directly before a line break is RTF's hard return,
				// which readControl reports as the newline itself.
				write("\n")
			case "tab", "cell":
				write("\t")
			case "row":
				write("\n")
			case "uc":
				n, err := strconv.ParseUint(arg, 10, 16)
				if err != nil {
					return "", fmt.Errorf("invalid RTF Unicode fallback count")
				}
				state.fallback = int(n)
			case "u":
				// RTF carries UTF-16 code units, normally signed. Accept the
				// unsigned spelling too, but never wrap an oversized integer.
				n, err := strconv.ParseInt(arg, 10, 32)
				if err != nil || n < -32768 || n > 65535 {
					return "", fmt.Errorf("invalid RTF UTF-16 code unit")
				}
				if n < 0 {
					n += 65536
				}
				writeUnit(rune(n))
				if i, err = skipUnicodeFallback(s, i, state.fallback); err != nil {
					return "", err
				}
			case "'":
				// \'hh is a byte in the document code page.
				b, err := rtfHexByte(arg)
				if err != nil {
					return "", err
				}
				flushSurrogate()
				sb.WriteRune(cp1252Rune(b))
			case "\\", "{", "}":
				write(word)
			case "~":
				write("\u00A0")
			case "_":
				write("\u2011")
			case "-":
				write("\u00AD")
			default:
				if skipDestinations[word] {
					state.skip = true
				}
			}
		default:
			// Raw line breaks are formatting of the RTF source, not of the text.
			if !state.skip && c != '\r' && c != '\n' {
				flushSurrogate()
				sb.WriteByte(c)
			}
			i++
		}
	}
	if i == len(s) && len(stack) != 0 {
		return "", fmt.Errorf("unclosed RTF group")
	}
	flushSurrogate()

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

// skipUnicodeFallback counts plain bytes and complete escapes, not source
// characters in an escape. A group boundary terminates the fallback.
func skipUnicodeFallback(s string, i, count int) (int, error) {
	for count > 0 && i < len(s) {
		switch s[i] {
		case '{', '}':
			return i, nil
		case '\r', '\n':
			i++
			continue
		case '\\':
			word, arg, next := readControl(s, i)
			i = next
			switch word {
			case "'":
				if _, err := rtfHexByte(arg); err != nil {
					return i, err
				}
			case "bin":
				var err error
				if i, err = skipRTFBinary(s, i, arg); err != nil {
					return i, err
				}
			}
		default:
			i++
		}
		count--
	}
	return i, nil
}

func rtfHexByte(arg string) (byte, error) {
	isHex := func(c byte) bool {
		return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	if len(arg) != 2 || !isHex(arg[0]) || !isHex(arg[1]) {
		return 0, fmt.Errorf("invalid RTF hexadecimal byte")
	}
	n, err := strconv.ParseUint(arg, 16, 8)
	if err != nil {
		return 0, fmt.Errorf("invalid RTF hexadecimal byte: %w", err)
	}
	return byte(n), nil
}

func skipRTFBinary(s string, i int, arg string) (int, error) {
	n, err := strconv.ParseUint(arg, 10, 32)
	if err != nil || n > uint64(len(s)-i) {
		return i, fmt.Errorf("invalid RTF binary length")
	}
	return i + int(n), nil
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
