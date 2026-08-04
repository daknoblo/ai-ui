package server

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/daknoblo/ai-ui/internal/logbuf"
)

// logData is the data of the log page.
type logData struct {
	Title string
	Lines []string
	Level string
}

// handleLogs renders the log page.
func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "logs", logData{
		Title: s.t("logs.title"),
		Lines: s.logs.Lines(),
		Level: strings.ToLower(s.logs.Level().String()),
	})
}

// handleLogTail returns only the log lines so the page can refresh itself.
func (s *Server) handleLogTail(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "log-lines", logData{Lines: s.logs.Lines()})
}

// handleLogClear empties the buffer.
func (s *Server) handleLogClear(w http.ResponseWriter, _ *http.Request) {
	s.logs.Clear()
	slog.Info("log buffer cleared")
	s.render(w, "log-lines", logData{Lines: s.logs.Lines()})
}

// applyLogLevel switches the runtime log level to the configured value.
func (s *Server) applyLogLevel(level string) {
	s.logs.SetLevel(logbuf.ParseLevel(level))
}
