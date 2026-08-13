package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
)

// TestStreamRetriesWithoutTemperature covers the reasoning models that only
// accept their default temperature: the request is repeated without the field
// instead of failing.
func TestStreamRetriesWithoutTemperature(t *testing.T) {
	var bodies []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		bodies = append(bodies, body)

		if _, ok := body["temperature"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'temperature' does not support 0.7 with this model.","param":"temperature"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = srv.URL + "/openai/v1"
	cfg.ChatDeployment = "gpt-5.6-sol"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}

	var got string
	if _, err := New(store).ChatStream(context.Background(), ChatOptions{}, []Message{{Role: "user", Content: "ping"}},
		func(delta string) error {
			got += delta
			return nil
		}); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected a retry, got %d requests", len(bodies))
	}
	if _, ok := bodies[0]["temperature"]; !ok {
		t.Error("the first request must carry the configured temperature")
	}
	if _, ok := bodies[1]["temperature"]; ok {
		t.Error("the retry must omit the temperature")
	}
	if got != "hi" {
		t.Errorf("streamed content = %q, want hi", got)
	}
}

// TestStreamRetriesWithoutReasoningEffort covers the opposite case: a model
// without reasoning support rejects the configured effort, so the request is
// repeated without it.
func TestStreamRetriesWithoutReasoningEffort(t *testing.T) {
	var bodies []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		bodies = append(bodies, body)

		if _, ok := body["reasoning_effort"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: reasoning_effort","param":"reasoning_effort"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = srv.URL + "/openai/v1"
	cfg.ChatDeployment = "gpt-4.1"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}

	if _, err := New(store).ChatStream(context.Background(), ChatOptions{ReasoningEffort: "high"},
		[]Message{{Role: "user", Content: "ping"}}, func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected a retry, got %d requests", len(bodies))
	}
	if bodies[0]["reasoning_effort"] != "high" {
		t.Errorf("the first request must carry the configured effort, got %v", bodies[0]["reasoning_effort"])
	}
	if _, ok := bodies[1]["reasoning_effort"]; ok {
		t.Error("the retry must omit the reasoning effort")
	}
}

// TestStreamOmitsAutomaticReasoningEffort makes sure a chat without a chosen
// effort does not send the parameter at all.
func TestStreamOmitsAutomaticReasoningEffort(t *testing.T) {
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = srv.URL + "/openai/v1"
	cfg.ChatDeployment = "gpt-4.1"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}

	if _, err := New(store).ChatStream(context.Background(), ChatOptions{ReasoningEffort: ReasoningAuto},
		[]Message{{Role: "user", Content: "ping"}}, func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("an automatic reasoning effort must not be sent")
	}
}

// TestClassicStreamUsesSelectedDeployment covers the classic schema, where the
// URL path decides which deployment answers.
func TestClassicStreamUsesSelectedDeployment(t *testing.T) {
	var path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = srv.URL
	cfg.ChatDeployment = "model-router"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	client := New(store)

	if _, err := client.ChatStream(context.Background(), ChatOptions{Model: "o4-mini"},
		[]Message{{Role: "user", Content: "ping"}}, func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if want := "/openai/deployments/o4-mini/chat/completions"; path != want {
		t.Errorf("request path = %q, want %q", path, want)
	}

	if _, err := client.ChatStream(context.Background(), ChatOptions{},
		[]Message{{Role: "user", Content: "ping"}}, func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if want := "/openai/deployments/model-router/chat/completions"; path != want {
		t.Errorf("without a selection the path = %q, want %q", path, want)
	}
}

// TestEmbedSendsDeploymentOnV1 makes sure the embedding deployment travels in
// the body on the v1 surface, where the path does not carry it.
func TestEmbedSendsDeploymentOnV1(t *testing.T) {
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = srv.URL + "/openai/v1"
	cfg.ChatDeployment = "model-router"
	cfg.EmbeddingDeployment = "text-embedding-3-large"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}

	if _, err := New(store).Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if body["model"] != "text-embedding-3-large" {
		t.Errorf("model = %v, want the embedding deployment", body["model"])
	}
}
