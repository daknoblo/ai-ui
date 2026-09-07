package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

type llmIdentity struct{}

func (llmIdentity) Refresh(context.Context, string) (foundry.Snapshot, error) {
	return foundry.Snapshot{}, fmt.Errorf("discovery is not used by inference")
}

func (llmIdentity) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer fake-identity")
	return nil
}

const llmResourceID = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/test"

func identityClient(t *testing.T, endpoint string) (*Client, *config.Store) {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"),
		config.Keys{API: "must-not-be-used"}, config.Overrides{})
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	store.ConfigureFoundry(llmResourceID, llmIdentity{}, nil)
	if err := store.SetCatalog(foundry.Snapshot{
		ResourceID: llmResourceID, Endpoint: endpoint + "/openai/v1",
		Deployments: []foundry.Deployment{
			{Name: "talk", ModelName: "gpt-4o", ModelVersion: "2024-08-06", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			{Name: "text-only", ModelName: "o3-mini", ModelVersion: "2025-01-31", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			{Name: "old-vectors", ModelName: "text-embedding-3-small", ModelVersion: "1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			{Name: "new-vectors", ModelName: "text-embedding-3-large", ModelVersion: "1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			{Name: "draw", ModelName: "gpt-image-1", ModelVersion: "1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.ChatDeployment, cfg.EmbeddingDeployment, cfg.ImageDeployment = "talk", "new-vectors", "draw"
	cfg.APIVersion = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return New(store), store
}

func TestFoundryInferenceUsesIdentityForEveryOperation(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fake-identity" || r.Header.Get("api-key") != "" {
			t.Error("request must use Entra without an API key")
		}
		if strings.HasSuffix(r.URL.Path, "/images/edits") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer func() { _ = r.MultipartForm.RemoveAll() }() // remove test upload scratch files
			if r.FormValue("model") != "draw" {
				t.Error("image edit did not use deployment alias")
			}
			if _, err := fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString([]byte("image"))); err != nil {
				t.Error(err)
			}
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			if request.Model != "new-vectors" {
				t.Errorf("embedding model = %q", request.Model)
			}
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.25,0.75]}]}`))
		case strings.HasSuffix(r.URL.Path, "/images/generations"):
			if request.Model != "draw" {
				t.Errorf("image model = %q", request.Model)
			}
			if _, err := fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString([]byte("image"))); err != nil {
				t.Error(err)
			}
		default:
			if request.Model != "talk" {
				t.Errorf("chat model = %q", request.Model)
			}
			if request.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
			} else {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
			}
		}
	}))
	defer azure.Close()
	client, _ := identityClient(t, azure.URL)
	var answer strings.Builder
	if _, err := client.ChatStream(t.Context(), ChatOptions{}, []Message{{Role: "user", Content: "hello"}},
		func(delta string) error { answer.WriteString(delta); return nil }); err != nil {
		t.Fatal(err)
	}
	if answer.String() != "hello" {
		t.Fatalf("stream = %q", answer.String())
	}
	if err := client.VerifyChat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyEmbedding(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GenerateImage(t.Context(), "test", ImageOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EditImage(t.Context(), "test", ImageSource{Name: "image.png", MIME: "image/png", Data: []byte("image")}, ImageOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 {
		t.Fatalf("requests = %v", paths)
	}
}

func TestFoundryEmbeddingUsesActiveProfileNotPendingSelection(t *testing.T) {
	var calls atomic.Int64
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "old-vectors" {
			t.Errorf("active index queried with pending model %q", request.Model)
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.25,0.75]}]}`))
	}))
	defer azure.Close()
	client, _ := identityClient(t, azure.URL)
	profile := storage.EmbeddingProfile{
		ResourceID: strings.ToLower(llmResourceID), Endpoint: azure.URL + "/openai/v1",
		Deployment: "old-vectors", ModelName: "text-embedding-3-small", ModelVersion: "1", Dimensions: 2,
	}
	if _, err := client.EmbedProfile(t.Context(), profile, []string{"question"}); err != nil {
		t.Fatal(err)
	}
	profile.ModelVersion = "outdated"
	if _, err := client.EmbedProfile(t.Context(), profile, []string{"question"}); err == nil || calls.Load() != 1 {
		t.Fatal("changed model version was not rejected before inference")
	}
}

func TestFoundryVisionUsesMetadataAndExplicitFallback(t *testing.T) {
	client, store := identityClient(t, "http://127.0.0.1:1")
	if name, ok := client.VisionDeployment("talk"); !ok || name != "talk" {
		t.Fatal("custom vision alias was not resolved")
	}
	if _, ok := client.VisionDeployment("text-only"); ok {
		t.Fatal("text-only alias was guessed to support vision")
	}
	cfg := store.Get()
	cfg.VisionDeployment = "talk"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if name, ok := client.VisionDeployment("text-only"); !ok || name != "talk" {
		t.Fatal("explicit vision selection was not honored")
	}
}
