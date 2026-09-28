package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func (s *Server) imageModels() []string {
	if s.cfg.ImageFoundryStatus().Enabled {
		return s.cfg.ImageModels(foundry.Images)
	}
	return s.cfg.Get().ImageModels
}

func (s *Server) validateImageChoice(model string, edit bool) error {
	if s.cfg.ImageFoundryStatus().Enabled {
		op := foundry.Images
		if edit {
			op = foundry.ImageEdits
		}
		return s.cfg.ValidateImageSelection(op, model)
	}
	if model != "" && !slices.Contains(s.cfg.Get().ImageModels, model) {
		return fmt.Errorf("image model is unavailable")
	}
	return nil
}

func (s *Server) handleSetImageModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, s.t("error.invalid_model"), http.StatusBadRequest)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetChat(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	model := strings.TrimSpace(r.FormValue("image_model"))
	if err := s.validateImageChoice(model, false); err != nil {
		http.Error(w, s.t("error.invalid_model"), http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateChatImageModel(r.Context(), id, model); err != nil {
		s.httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
