package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConversationScrollControls(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		for _, history := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/history=%t", language, history), func(t *testing.T) {
				s, handler, _, _ := newFoundryTestServer(t, language)
				id, err := s.store.CreateChat(t.Context(), "Reading test", "chat-prod", "auto")
				if err != nil {
					t.Fatal(err)
				}
				if history {
					if _, err := s.store.AddMessage(t.Context(), id, "assistant", "Existing answer"); err != nil {
						t.Fatal(err)
					}
				}
				result := httptest.NewRecorder()
				handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
				body := result.Body.String()
				for _, expected := range []string{
					`src="/static/chat-scroll.js?v=`, `id="messages"`, `tabindex="0"`,
					`id="scroll-status"`, `id="scroll-to-latest"`, `aria-controls="messages"`,
					`aria-live="polite"`, s.t("chat.scroll_finished"), s.t("chat.scroll_reconnecting"),
				} {
					if !strings.Contains(body, expected) {
						t.Errorf("scroll controls omit %q", expected)
					}
				}
				if strings.Contains(body, "function scrollMessages") || strings.Contains(body, "chat.scroll_") {
					t.Fatal("unconditional scrolling or untranslated scroll controls remain")
				}
				if strings.Count(body, `id="scroll-to-latest"`) != 1 {
					t.Fatal("the conversation must contain exactly one jump control")
				}
			})
		}
	}
}
