package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestResponseFooterRetainsReportedModelAndUsage(t *testing.T) {
	s, handler, _, _ := newFoundryTestServer(t, "en")
	chatID, err := s.store.CreateChat(t.Context(), "Footer test", "configured-but-not-used", "auto")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.store.CreateGeneration(t.Context(), chatID, "Question", "{}", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	model := `actual-model<&">`
	usage := "400 tokens · 385 input / 15 output"
	snapshot, err := json.Marshal(map[string]string{
		"model": s.renderString("model-tag", model), "usage": usage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.FinishGeneration(t.Context(), turn.ID, storage.GenerationCompleted,
		"Answer", string(snapshot), "", nil, nil); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", chatID), nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, usage) ||
		!strings.Contains(body, "actual-model&lt;&amp;") || strings.Contains(body, model) {
		t.Fatalf("saved footer missing or unsafe: %d %s", response.Code, body)
	}
	_, footer, _ := strings.Cut(body, `<div class="msg-metadata">`)
	footer, _, _ = strings.Cut(footer, "</div>")
	if !strings.Contains(footer, `class="msg-usage"`) || !strings.Contains(footer, `class="msg-model"`) ||
		!strings.Contains(footer, "actual-model") || strings.Contains(footer, "configured-but-not-used") {
		t.Fatalf("model must use the recorded response in the usage row: %s", footer)
	}
	if !strings.Contains(body, `<span class="response-retry-label">Retry</span>`) ||
		!strings.Contains(body, fmt.Sprintf(`hx-post="/chat/%d/retry/%d"`, chatID, turn.ID)) {
		t.Fatal("completed response is missing its visibly labeled retry control")
	}
}

func TestResponseMetadataLegacyAndInvalidSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, snapshot, model, usage string
		wantErr                      bool
	}{
		{"empty", "", "", "", false},
		{"legacy tag", `{"model":" <span class=\"model-badge\" title=\"Used model\">gpt-6-astra</span>","usage":"42 tokens"}`, "gpt-6-astra", "42 tokens", false},
		{"usage only", `{"usage":"42 tokens"}`, "", "42 tokens", false},
		{"model only", `{"model":"<span>reported-model</span>"}`, "reported-model", "", false},
		{"invalid JSON", "{", "", "", true},
		{"invalid label", `{"model":"not a span","usage":"42 tokens"}`, "", "42 tokens", true},
		{"unsafe root", `{"model":"<script>alert(1)</script>"}`, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, usage, err := responseMetadata(tc.snapshot)
			if (err != nil) != tc.wantErr || model != tc.model || usage != tc.usage {
				t.Fatalf("metadata = %q, %q, %v", model, usage, err)
			}
		})
	}
}
