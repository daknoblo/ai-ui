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
