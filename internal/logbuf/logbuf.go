// Package logbuf keeps the most recent log lines in memory so the UI can show
// them without access to the container log.
package logbuf

import (
	"log/slog"
	"strings"
	"sync"
)

// Buffer is an io.Writer for a slog handler plus the level switch of that
// handler. It keeps at most max lines and drops the oldest ones.
type Buffer struct {
	mu    sync.Mutex
	lines []string
	max   int

	level slog.LevelVar
}

// New creates a buffer for at most max lines.
func New(max int) *Buffer {
	if max <= 0 {
		max = 1000
	}
	b := &Buffer{max: max, lines: make([]string, 0, max)}
	b.level.Set(slog.LevelInfo)
	return b
}

// Write stores the log record; it is called by the slog handler.
func (b *Buffer) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		b.lines = append(b.lines, line)
	}
	if overflow := len(b.lines) - b.max; overflow > 0 {
		b.lines = append(b.lines[:0], b.lines[overflow:]...)
	}
	return len(p), nil
}

// Lines returns a copy of the buffered lines, oldest first.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

// Clear discards all buffered lines.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = b.lines[:0]
}

// LevelVar exposes the level switch for the slog handler options.
func (b *Buffer) LevelVar() *slog.LevelVar {
	return &b.level
}

// SetLevel changes the active log level at runtime.
func (b *Buffer) SetLevel(l slog.Level) {
	b.level.Set(l)
}

// Level returns the active log level.
func (b *Buffer) Level() slog.Level {
	return b.level.Level()
}

// Levels are the selectable log levels (the first entry is the fallback).
var Levels = []string{"info", "debug", "warn", "error"}

// ParseLevel maps a configured name to a slog level.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NormalizeLevel keeps a configured value within Levels.
func NormalizeLevel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, l := range Levels {
		if name == l {
			return name
		}
	}
	return Levels[0]
}
