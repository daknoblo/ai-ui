package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
)

// TestIsChatModelName verifies that non-chat models (embeddings, image, audio)
// are reliably filtered out of the picker.
func TestIsChatModelName(t *testing.T) {
	chat := []string{"gpt-4o", "gpt-4o-mini", "o3", "o4-mini", "gpt-4.1"}
	for _, name := range chat {
		if !isChatModelName(name) {
			t.Errorf("expected a chat model but it was filtered out: %s", name)
		}
	}

	nonChat := []string{
		"text-embedding-3-large", "text-embedding-ada-002",
		"dall-e-3", "whisper", "tts-1", "gpt-4o-transcribe",
		"text-moderation-latest", "sora",
	}
	for _, name := range nonChat {
		if isChatModelName(name) {
			t.Errorf("expected filtering but it was accepted as a chat model: %s", name)
		}
	}
}

// TestListModelsUsesDeployments makes sure ListModels queries the deployments
// endpoint and returns only unique, sorted chat model names of successfully
// provisioned deployments.
func TestListModelsUsesDeployments(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("api-key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "chat-prod", "model": "gpt-4o", "status": "succeeded"},
				{"id": "chat-mini", "model": "gpt-4o-mini", "status": "succeeded"},
				{"id": "second-gpt4o", "model": "gpt-4o", "status": "succeeded"},
				{"id": "emb", "model": "text-embedding-3-large", "status": "succeeded"},
				{"id": "half-baked", "model": "o3", "status": "creating"}
			],
			"object": "list"
		}`))
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), "test-key", "", "", config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = srv.URL
	cfg.APIVersion = "2024-08-01-preview"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}

	models, err := New(store).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/openai/deployments") {
		t.Errorf("expected a request to /openai/deployments, got %q", gotPath)
	}

	// Expected: embeddings filtered out, the not-yet-provisioned "o3"
	// (creating) ignored, the duplicate gpt-4o deduplicated, result sorted.
	want := []string{"gpt-4o", "gpt-4o-mini"}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("unexpected models: got %v, want %v", models, want)
	}
}

// TestChatCompletionsURL checks the schema detection (classic vs. v1) for the
// chat completions URL.
func TestChatCompletionsURL(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		deployment string
		apiVersion string
		want       string
	}{
		{
			name:       "v1",
			endpoint:   "https://x.services.ai.azure.com/openai/v1",
			deployment: "model-router",
			apiVersion: "2025-01-01-preview",
			want:       "https://x.services.ai.azure.com/openai/v1/chat/completions",
		},
		{
			name:       "v1 with trailing slash",
			endpoint:   "https://x.services.ai.azure.com/openai/v1/",
			deployment: "model-router",
			apiVersion: "preview",
			want:       "https://x.services.ai.azure.com/openai/v1/chat/completions",
		},
		{
			name:       "classic",
			endpoint:   "https://x.openai.azure.com",
			deployment: "gpt-4o",
			apiVersion: "2024-08-01-preview",
			want:       "https://x.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-08-01-preview",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatCompletionsURL(tc.endpoint, tc.deployment, tc.apiVersion); got != tc.want {
				t.Errorf("chatCompletionsURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmbeddingsAndModelsURL checks the embeddings and models URLs per schema.
func TestEmbeddingsAndModelsURL(t *testing.T) {
	if got := embeddingsURL("https://x.services.ai.azure.com/openai/v1", "text-embedding-3-large", "v"); got != "https://x.services.ai.azure.com/openai/v1/embeddings" {
		t.Errorf("embeddingsURL v1 is wrong: %q", got)
	}
	if got := embeddingsURL("https://x.cognitiveservices.azure.com", "text-embedding-3-large", "2024-02-01"); got != "https://x.cognitiveservices.azure.com/openai/deployments/text-embedding-3-large/embeddings?api-version=2024-02-01" {
		t.Errorf("embeddingsURL classic is wrong: %q", got)
	}
	if got := modelsURL("https://x.services.ai.azure.com/openai/v1", "v"); got != "https://x.services.ai.azure.com/openai/v1/models" {
		t.Errorf("modelsURL v1 is wrong: %q", got)
	}
	if got := modelsURL("https://x.openai.azure.com", "2024-08-01-preview"); got != "https://x.openai.azure.com/openai/deployments?api-version=2024-08-01-preview" {
		t.Errorf("modelsURL classic is wrong: %q", got)
	}
}

// TestChatModelField verifies that with the v1 schema the deployment is used as
// the model, that a pinned model takes precedence and that the classic schema
// leaves the field empty (router decides).
func TestChatModelField(t *testing.T) {
	v1 := config.Config{Endpoint: "https://x.services.ai.azure.com/openai/v1", ChatDeployment: "model-router"}
	if got := chatModelField(v1); got != "model-router" {
		t.Errorf("v1 without a pinned model: got %q, want model-router", got)
	}
	v1.ChatModel = "gpt-4o"
	if got := chatModelField(v1); got != "gpt-4o" {
		t.Errorf("v1 with a pinned model: got %q, want gpt-4o", got)
	}
	classic := config.Config{Endpoint: "https://x.openai.azure.com", ChatDeployment: "model-router"}
	if got := chatModelField(classic); got != "" {
		t.Errorf("classic without a pinned model: got %q, want empty", got)
	}
}
