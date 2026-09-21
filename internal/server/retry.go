package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		http.Error(w, s.t("retry.unavailable"), http.StatusServiceUnavailable)
		return
	}
	if len(s.generations) >= maxConcurrentGenerations {
		w.Header().Set("Retry-After", "2")
		http.Error(w, s.t("stream.overloaded"), http.StatusServiceUnavailable)
		return
	}
	original, err := s.store.GetGeneration(r.Context(), chatID, turnID)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, s.t("retry.unavailable"), http.StatusNotFound)
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}
	var opts generationOptions
	if err := json.Unmarshal([]byte(original.Request), &opts); err != nil {
		slog.Error("decode retry request", "turn", turnID, "err", err)
		http.Error(w, s.t("retry.unavailable"), http.StatusConflict)
		return
	}
	// Never replace an original edit source with a newer generated/uploaded image.
	if opts.Image && opts.Edit {
		source, err := s.store.GetImage(r.Context(), opts.SourceImageID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.httpError(w, err)
			return
		}
		if errors.Is(err, storage.ErrNotFound) || source.ChatID != chatID {
			http.Error(w, s.t("stream.image_source_missing"), http.StatusConflict)
			return
		}
	}
	if err := s.store.InterruptExpiredGenerations(r.Context(), time.Now()); err != nil {
		s.httpError(w, err)
		return
	}
	turn, err := s.store.RetryGeneration(r.Context(), chatID, turnID, time.Now().Add(generationTimeout+10*time.Second))
	if errors.Is(err, storage.ErrGenerationBusy) {
		http.Error(w, s.t("stream.busy"), http.StatusConflict)
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, s.t("retry.unavailable"), http.StatusNotFound)
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}
	s.startGenerationLocked(turn, opts)
	s.render(w, "assistant-stream", streamView{ChatID: chatID, TurnID: turn.ID,
		Web: opts.Web, Image: opts.Image, Edit: opts.Edit, Retried: true})
}
