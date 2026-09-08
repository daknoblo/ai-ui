package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestChatHeaderHasNoModelSelector(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			s, handler, _, _ := newFoundryTestServer(t, language)
			cfg := s.cfg.Get()
			cfg.ChatDeployment, cfg.ImageDeployment = "chat-prod", "pictures-prod"
			if err := s.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			id, err := s.store.CreateChat(t.Context(), "Header test", "chat-prod", "auto")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.store.UpdateChatImageModel(t.Context(), id, "pictures-prod"); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{storage.ChatModeImage, storage.ChatModeChat} {
				response := postFoundryForm(handler, fmt.Sprintf("/chat/%d/mode", id), url.Values{"mode": {mode}})
				if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
					t.Fatalf("mode switch returned obsolete header content: %d %s", response.Code, response.Body.String())
				}
				page := httptest.NewRecorder()
				handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
				body := page.Body.String()
				_, afterHeader, found := strings.Cut(body, `<div class="chat-header">`)
				header, _, closed := strings.Cut(afterHeader, "</div>")
				if page.Code != http.StatusOK || !found || !closed ||
					!strings.Contains(header, `id="chat-title"`) || !strings.Contains(header, `id="nav-toggle"`) {
					t.Fatal("chat title or mobile navigation was lost")
				}
				if strings.Contains(header, "<select") || strings.Contains(body, "model-picker") || strings.Contains(body, "model-select") {
					t.Fatal("the removed model selector remains in the page")
				}
				for _, expected := range []string{`id="reasoning-opt"`, `id="image-params"`, `data-image-edits="1"`} {
					if !strings.Contains(body, expected) {
						t.Errorf("removing the header selector removed composer support: %s", expected)
					}
				}
				chat, err := s.store.GetChat(t.Context(), id)
				if err != nil || chat.Mode != mode || chat.Model != "chat-prod" || chat.ImageModel != "pictures-prod" {
					t.Fatalf("mode switch changed the saved model selections: %+v, %v", chat, err)
				}
			}
			settings := httptest.NewRecorder()
			handler.ServeHTTP(settings, httptest.NewRequest(http.MethodGet, "/config", nil))
			for _, field := range []string{`name="chat_deployment"`, `name="image_deployment"`} {
				if !strings.Contains(settings.Body.String(), field) {
					t.Errorf("settings model selection was removed: %s", field)
				}
			}
		})
	}
}

func TestResponseModelBadgeRemainsWithoutHeaderSelector(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"model\":\"actual-backend-model\",\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n") // Local stream fixture.
	}))
	t.Cleanup(backend.Close)
	_, handler, id := generationTestServer(t, backend.URL)
	streamURL := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Hello"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, streamURL, nil))
	for _, expected := range []string{"event: model", `class="model-badge"`, "actual-backend-model"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("responding-model display is missing %q: %s", expected, response.Body.String())
		}
	}
}
