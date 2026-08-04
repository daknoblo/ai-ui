package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
)

// TestImagesURL checks the schema detection (classic vs. v1) for the image URL.
func TestImagesURL(t *testing.T) {
	if got := imagesURL("https://x.services.ai.azure.com/openai/v1", "gpt-image-2", "v"); got != "https://x.services.ai.azure.com/openai/v1/images/generations" {
		t.Errorf("imagesURL v1 is wrong: %q", got)
	}
	if got := imagesURL("https://x.openai.azure.com", "gpt-image-2", "2025-04-01-preview"); got != "https://x.openai.azure.com/openai/deployments/gpt-image-2/images/generations?api-version=2025-04-01-preview" {
		t.Errorf("imagesURL classic is wrong: %q", got)
	}
}

// TestGenerateImage verifies the request body of the v1 schema, the decoding of
// the base64 payload and the content type derived from the output format.
func TestGenerateImage(t *testing.T) {
	want := []byte("fake-image-bytes")
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "img-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(want)}},
			"usage": map[string]int{"input_tokens": 12, "output_tokens": 30, "total_tokens": 42},
		})
	}))
	defer srv.Close()

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "chat-key", Image: "img-key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = "https://chat.example"
	cfg.ImageEndpoint = srv.URL + "/openai/v1"
	cfg.ImageDeployment = "gpt-image-2"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}

	res, err := New(store).GenerateImage(context.Background(), "a cat",
		ImageOptions{Size: "1024x1024", Quality: "auto", Format: "webp"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}

	if body["model"] != "gpt-image-2" {
		t.Errorf("the v1 schema must send the deployment as model: %v", body["model"])
	}
	if body["size"] != "1024x1024" {
		t.Errorf("size was not passed through: %v", body["size"])
	}
	if _, ok := body["quality"]; ok {
		t.Errorf(`"auto" must be omitted, but quality was sent: %v`, body["quality"])
	}
	if body["output_format"] != "webp" {
		t.Errorf("output_format was not passed through: %v", body["output_format"])
	}
	if string(res.Data) != string(want) {
		t.Errorf("unexpected image data: %q", res.Data)
	}
	if res.MIME != "image/webp" {
		t.Errorf("MIME = %q, want image/webp", res.MIME)
	}
	if res.Usage.TotalTokens != 42 || res.Usage.PromptTokens != 12 || res.Usage.CompletionTokens != 30 {
		t.Errorf("usage was not mapped: %+v", res.Usage)
	}
}

// TestGenerateImageRequiresDeployment makes sure an unconfigured image endpoint
// fails before a request is sent.
func TestGenerateImageRequiresDeployment(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "chat-key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint = "https://chat.example"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	if _, err := New(store).GenerateImage(context.Background(), "a cat", ImageOptions{}); err == nil {
		t.Error("a missing deployment must be rejected")
	}
}
