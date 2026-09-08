package docparse

import (
	"strings"
	"testing"
)

func TestRTFUnicodeAndFallback(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"literal fallback", `{\rtf1\ansi\uc1 caf\u233e}`, "café"},
		{"default fallback", `{\rtf1 caf\u233e}`, "café"},
		{"no fallback", `{\rtf1\uc0\u233?}`, "é?"},
		{"multiple fallbacks", `{\rtf1\uc2\u233ab!}`, "é!"},
		{"hex fallback", `{\rtf1\u233\'e9!}`, "é!"},
		{"escaped brace fallback", `{\rtf1\u233\{!}`, "é!"},
		{"escaped backslash fallback", `{\rtf1\u233\\!}`, "é!"},
		{"control fallback", `{\rtf1\u233\~!}`, "é!"},
		{"mixed fallback", `{\rtf1\uc3\u233a\'e9\}!}`, "é!"},
		{"source newline", "{\\rtf1\\u233\r\nx!}", "é!"},
		{"group boundary", `{\rtf1\uc2\u233{X}Y}`, "éXY"},
		{"scoped fallback", `{\rtf1\uc1{\uc0\u233?}\u233x!}`, "é?é!"},
		{"scoped inherited fallback", `{\rtf1\uc2{\u233ab}\u233cd!}`, "éé!"},
		{"signed surrogate pair", `{\rtf1\u-10179?\u-8704?}`, "😀"},
		{"unsigned surrogate pair", `{\rtf1\u55357?\u56832?}`, "😀"},
		{"scoped surrogate pair", `{\rtf1{\u-10179?}{\u-8704?}}`, "😀"},
		{"surrogate with formatting", `{\rtf1\u-10179?\b\u-8704?}`, "😀"},
		{"unpaired high", `{\rtf1\u-10179?}`, "�"},
		{"unpaired low", `{\rtf1\u-8704?}`, "�"},
		{"interrupted pair", `{\rtf1\u-10179?X\u-8704?}`, "�X�"},
		{"successive high", `{\rtf1\u-10179?\u-10179?\u-8704?}`, "�😀"},
		{"upper code unit", `{\rtf1\u65535?}`, "\uFFFF"},
		{"lower signed code unit", `{\rtf1\u-32768?}`, "\u8000"},
		{"code page compatibility", `{\rtf1\ansi\ansicpg1252 Gr\'fc\'df \u233e \'80}`, "Grüß é €"},
		{"nested skipped destination", `{\rtf1{\fonttbl a{\*\private hidden}b}Body}`, "Body"},
		{"binary skipped destination", `{\rtf1{\pict\bin3 {\}}Body}`, "Body"},
		{"binary fallback", `{\rtf1\u233\bin3 {\}!}`, "é!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Extract("unicode.rtf", "", []byte(tt.input))
			if err != nil || got != tt.want {
				t.Fatalf("Extract = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestRTFRejectsMalformedControls(t *testing.T) {
	for _, input := range []string{
		`{\rtf1\u4294967361?}`, `{\rtf1\u2147483648?}`, `{\rtf1\u65536?}`,
		`{\rtf1\u-32769?}`, `{\rtf1\u999999999999999999999999?}`,
		`{\rtf1\u?}`, `{\rtf1\u+233?}`,
		`{\rtf1\uc-1\u233?}`, `{\rtf1\uc65536\u233?}`,
		`{\rtf1\'-1}`, `{\rtf1\'+1}`, `{\rtf1\'fg}`, `{\rtf1\'f}`,
		`{\rtf1\u233\'-1}`, `{\rtf1\bin-1 x}`, `{\rtf1\bin100 x}`,
		`{\rtf1\bin2147483648 x}`, `{\rtf1\bin4294967295 x}`,
		`{\rtf1\bin18446744073709551616 x}`,
		`{\rtf1 text`, `{\rtf1 text}}`,
		strings.Repeat("{", 257) + "text" + strings.Repeat("}", 257),
	} {
		t.Run(input[:min(len(input), 50)], func(t *testing.T) {
			got, err := Extract("invalid.rtf", "", []byte(input))
			if err == nil || got != "" {
				t.Fatalf("Extract = %q, %v; want an error without partial text", got, err)
			}
		})
	}
}

func TestRTFHexByteRequiresTwoUnsignedDigits(t *testing.T) {
	for _, arg := range []string{"", "f", "fff", "-1", "+1", "gg", " 1", "1 "} {
		if _, err := rtfHexByte(arg); err == nil {
			t.Errorf("rtfHexByte(%q) accepted malformed hexadecimal", arg)
		}
	}
	for arg, want := range map[string]byte{"00": 0, "7F": 127, "aB": 171, "ff": 255} {
		got, err := rtfHexByte(arg)
		if err != nil || got != want {
			t.Errorf("rtfHexByte(%q) = %d, %v; want %d", arg, got, err, want)
		}
	}
}
