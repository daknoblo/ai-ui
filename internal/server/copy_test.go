package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestResponseCopyControls(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			s, handler, _, _ := newFoundryTestServer(t, language)
			id, err := s.store.CreateChat(t.Context(), "Copy test", "chat-prod", "auto")
			if err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"user", "assistant"} {
				if _, err := s.store.AddMessage(t.Context(), id, role, "# Heading\n\n**Formatted** answer"); err != nil {
					t.Fatal(err)
				}
			}
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
			body := result.Body.String()
			if result.Code != http.StatusOK || strings.Count(body, `class="response-copy"`) != 2 ||
				strings.Count(body, `class="response-retry"`) != 2 ||
				strings.Count(body, `data-retry-unavailable="0"`) != 2 {
				t.Fatalf("expected copy and retry on legacy input and output: %d", result.Code)
			}
			for _, expected := range []string{
				`src="/static/copy-response.js?v=`, s.t("copy.response"),
				s.t("copy.failed"), `aria-live="polite"`,
			} {
				if !strings.Contains(body, expected) {
					t.Errorf("copy controls omit %q", expected)
				}
			}
			stream := s.renderString("assistant-stream", streamView{ChatID: id, TurnID: 1, QuestionID: 1})
			if !strings.Contains(stream, `data-stream-state="pending"`) ||
				strings.Count(stream, `class="response-copy"`) != 1 ||
				strings.Index(stream, `class="response-copy"`) < strings.Index(stream, `class="bubble"`) {
				t.Fatal("stream copy control must be after the response and gated by completion")
			}
			user := s.renderString("message", storage.Message{ID: 1, ChatID: id, QuestionID: 1, Role: "user", Content: "Question"})
			if !strings.Contains(user, `class="response-copy"`) ||
				!strings.Contains(user, fmt.Sprintf(`hx-post="/chat/%d/retry/1"`, id)) {
				t.Fatal("user messages must contain copy and retry controls")
			}
		})
	}
}
