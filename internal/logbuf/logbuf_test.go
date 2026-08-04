package logbuf

import (
	"log/slog"
	"strings"
	"testing"
)

// TestBufferKeepsNewestLines checks the ring behavior and the line splitting.
func TestBufferKeepsNewestLines(t *testing.T) {
	b := New(3)
	if _, err := b.Write([]byte("one\ntwo\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := b.Write([]byte("three\nfour\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := strings.Join(b.Lines(), ",")
	if got != "two,three,four" {
		t.Errorf("lines = %q, want two,three,four", got)
	}

	b.Clear()
	if len(b.Lines()) != 0 {
		t.Errorf("Clear did not empty the buffer: %v", b.Lines())
	}
}

// TestLevels verifies the mapping of the configured names.
func TestLevels(t *testing.T) {
	if got := ParseLevel("DEBUG"); got != slog.LevelDebug {
		t.Errorf("ParseLevel(DEBUG) = %v", got)
	}
	if got := ParseLevel("nonsense"); got != slog.LevelInfo {
		t.Errorf("unknown levels must fall back to info, got %v", got)
	}
	if got := NormalizeLevel("WARN"); got != "warn" {
		t.Errorf("NormalizeLevel(WARN) = %q", got)
	}
	if got := NormalizeLevel(""); got != Levels[0] {
		t.Errorf("NormalizeLevel(\"\") = %q, want %q", got, Levels[0])
	}
}
