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

// TestIsChatModelName prüft, dass Nicht-Chat-Modelle (Embeddings, Bild, Audio)
// zuverlässig aus der Auswahl gefiltert werden.
func TestIsChatModelName(t *testing.T) {
	chat := []string{"gpt-4o", "gpt-4o-mini", "o3", "o4-mini", "gpt-4.1"}
	for _, name := range chat {
		if !isChatModelName(name) {
			t.Errorf("erwartet Chat-Modell, wurde aber gefiltert: %s", name)
		}
	}

	nonChat := []string{
		"text-embedding-3-large", "text-embedding-ada-002",
		"dall-e-3", "whisper", "tts-1", "gpt-4o-transcribe",
		"text-moderation-latest", "sora",
	}
	for _, name := range nonChat {
		if isChatModelName(name) {
			t.Errorf("erwartet Filterung, wurde aber als Chat-Modell akzeptiert: %s", name)
		}
	}
}

// TestListModelsUsesDeployments stellt sicher, dass ListModels den
// Deployments-Endpoint abfragt und nur eindeutige, sortierte Chat-Modellnamen
// der erfolgreich bereitgestellten Deployments liefert.
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
		t.Fatalf("konfiguration speichern: %v", err)
	}

	models, err := New(store).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/openai/deployments") {
		t.Errorf("erwartet Abfrage von /openai/deployments, war aber %q", gotPath)
	}

	// Erwartet: Embedding gefiltert, nicht bereitgestelltes "o3" (creating)
	// ignoriert, doppeltes gpt-4o dedupliziert, Ergebnis sortiert.
	want := []string{"gpt-4o", "gpt-4o-mini"}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("unerwartete Modelle: got %v, want %v", models, want)
	}
}

// TestChatCompletionsURL prüft die Schema-Erkennung (klassisch vs. v1) für die
// Chat-Completions-URL.
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
			name:       "v1 mit Slash",
			endpoint:   "https://x.services.ai.azure.com/openai/v1/",
			deployment: "model-router",
			apiVersion: "preview",
			want:       "https://x.services.ai.azure.com/openai/v1/chat/completions",
		},
		{
			name:       "klassisch",
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

// TestEmbeddingsAndModelsURL prüft die Embeddings- und Modell-URLs je Schema.
func TestEmbeddingsAndModelsURL(t *testing.T) {
	if got := embeddingsURL("https://x.services.ai.azure.com/openai/v1", "text-embedding-3-large", "v"); got != "https://x.services.ai.azure.com/openai/v1/embeddings" {
		t.Errorf("embeddingsURL v1 falsch: %q", got)
	}
	if got := embeddingsURL("https://x.cognitiveservices.azure.com", "text-embedding-3-large", "2024-02-01"); got != "https://x.cognitiveservices.azure.com/openai/deployments/text-embedding-3-large/embeddings?api-version=2024-02-01" {
		t.Errorf("embeddingsURL klassisch falsch: %q", got)
	}
	if got := modelsURL("https://x.services.ai.azure.com/openai/v1", "v"); got != "https://x.services.ai.azure.com/openai/v1/models" {
		t.Errorf("modelsURL v1 falsch: %q", got)
	}
	if got := modelsURL("https://x.openai.azure.com", "2024-08-01-preview"); got != "https://x.openai.azure.com/openai/deployments?api-version=2024-08-01-preview" {
		t.Errorf("modelsURL klassisch falsch: %q", got)
	}
}

// TestChatModelField prüft, dass beim v1-Schema das Deployment als model dient,
// ein erzwungenes Modell aber Vorrang hat und klassisch leer bleibt (Router).
func TestChatModelField(t *testing.T) {
	v1 := config.Config{Endpoint: "https://x.services.ai.azure.com/openai/v1", ChatDeployment: "model-router"}
	if got := chatModelField(v1); got != "model-router" {
		t.Errorf("v1 ohne Force: got %q, want model-router", got)
	}
	v1.ChatModel = "gpt-4o"
	if got := chatModelField(v1); got != "gpt-4o" {
		t.Errorf("v1 mit Force: got %q, want gpt-4o", got)
	}
	classic := config.Config{Endpoint: "https://x.openai.azure.com", ChatDeployment: "model-router"}
	if got := chatModelField(classic); got != "" {
		t.Errorf("klassisch ohne Force: got %q, want leer", got)
	}
}
