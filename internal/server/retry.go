package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	chatID, err := parseID(r)
	turnID, turnErr := strconv.ParseInt(chi.URLParam(r, "turn"), 10, 64)
	if err != nil || turnErr != nil || chatID <= 0 || turnID <= 0 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, s.t("stream.limit"), http.StatusRequestEntityTooLarge)
		return
	}
	question, err := s.store.RetryQuestion(r.Context(), chatID, turnID)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, s.t("retry.unavailable"), http.StatusNotFound)
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}
	message := strings.TrimSpace(question.Content)
	if message == "" {
		http.Error(w, s.t("retry.unavailable"), http.StatusConflict)
		return
	}
	chat, err := s.store.GetChat(r.Context(), chatID)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, s.t("retry.unavailable"), http.StatusNotFound)
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}
	if !r.Form.Has("mode") {
		r.Form.Set("mode", chat.Mode)
	}
	s.sendMessage(w, r, chat, message)
}
