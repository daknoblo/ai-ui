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

// TestMessageWithoutImagesStaysAString makes sure the wire format is unchanged
// for the common case, so endpoints that do not implement the vision schema
// keep working.
func TestMessageWithoutImagesStaysAString(t *testing.T) {
	raw, err := json.Marshal(Message{Role: "user", Content: "ping"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("content is not a plain string: %v (%s)", err, raw)
	}
	if body.Role != "user" || body.Content != "ping" {
		t.Errorf("unexpected message: %s", raw)
	}
}

// TestMessageWithImagesUsesContentParts covers the multimodal format: attached
// images have to travel as inline data URLs next to the text.
func TestMessageWithImagesUsesContentParts(t *testing.T) {
	raw, err := json.Marshal(Message{
		Role:    "user",
		Content: "what is on it?",
		Images: []ImageContent{
			{MIME: "image/png", Data: []byte{1, 2, 3}},
			{MIME: "image/jpeg", Data: []byte{4}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var body struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("content is not an array of parts: %v (%s)", err, raw)
	}
	if len(body.Content) != 3 {
		t.Fatalf("got %d content parts, want text + 2 images: %s", len(body.Content), raw)
	}
	if body.Role != "user" {
		t.Errorf("the role was lost while switching to content parts: %s", raw)
	}
	if body.Content[0].Type != "text" || body.Content[0].Text != "what is on it?" {
		t.Errorf("the first part must carry the text: %s", raw)
	}
	if body.Content[1].Type != "image_url" || body.Content[1].ImageURL.URL != "data:image/png;base64,AQID" {
		t.Errorf("unexpected first image part: %s", raw)
	}
	if body.Content[2].ImageURL.URL != "data:image/jpeg;base64,BA==" {
		t.Errorf("unexpected second image part: %s", raw)
	}
}

// TestStreamSendsAttachedImages verifies the attachments survive all the way
// into the request body of a streamed answer.
func TestStreamSendsAttachedImages(t *testing.T) {
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
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

	messages := []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "describe it", Images: []ImageContent{{MIME: "image/png", Data: []byte{9}}}},
	}
	if _, err := New(store).ChatStream(context.Background(), ChatOptions{}, messages,
		func(string) error { return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	sent, _ := body["messages"].([]any)
	if len(sent) != 2 {
		t.Fatalf("got %d messages, want 2", len(sent))
	}
	system, _ := sent[0].(map[string]any)
	if _, ok := system["content"].(string); !ok {
		t.Errorf("a message without images must keep a string content: %v", system["content"])
	}
	user, _ := sent[1].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("the user message must carry content parts, got %T", user["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want text + image", len(parts))
	}
	image, _ := parts[1].(map[string]any)
	url, _ := image["image_url"].(map[string]any)
	if url["url"] != "data:image/png;base64,CQ==" {
		t.Errorf("image part = %v, want an inline data URL", parts[1])
	}
}
