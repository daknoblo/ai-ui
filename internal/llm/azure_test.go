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

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), "test-key", "", "")
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
