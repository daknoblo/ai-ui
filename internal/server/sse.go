package server

import (
	"fmt"
	"net/http"
	"strings"
)

// sseWriter wraps writing of server-sent events.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEWriter initializes the SSE response headers.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, true
}

// send writes a named SSE event. Multi-line data is encoded correctly (each
// line prefixed with "data: ").
func (s *sseWriter) send(event, data string) error {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\n")
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if _, err := fmt.Fprint(s.w, b.String()); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
