package demo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestDemoFoundrySource(t *testing.T) {
	source, err := newDemoFoundrySource("http://127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Refresh(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := foundry.ValidateResourceID(snapshot.ResourceID); err != nil {
		t.Fatal(err)
	}
	if snapshot.Endpoint != "http://127.0.0.1:8123/openai/v1" || snapshot.RefreshedAt.IsZero() {
		t.Fatalf("invalid demo snapshot: %+v", snapshot)
	}
	for op, names := range map[foundry.Operation][]string{
		foundry.Chat:       {"chat-primary", "chat-quick"},
		foundry.Embeddings: {"docs-primary", "docs-next"},
		foundry.Images:     {"canvas"},
		foundry.ImageEdits: {"canvas"},
		foundry.Vision:     {"chat-primary", "chat-quick"},
	} {
		got := snapshot.Names(op)
		for _, name := range names {
			if !slices.Contains(got, name) {
				t.Errorf("%s choices %v exclude %s", op, got, name)
			}
		}
		for _, name := range []string{"native-chat", "nightly-batch"} {
			if _, ok := snapshot.Find(name); !ok {
				t.Errorf("unsupported deployment %s is hidden from the inventory", name)
			}
			if slices.Contains(got, name) {
				t.Errorf("unsupported deployment %s is offered for %s", name, op)
			}
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, snapshot.Endpoint+"/embeddings", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("api-key", "unused-demo-key")
	if err := source.Authorize(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer demo-identity" || req.Header.Get("api-key") != "" {
		t.Fatal("demo authorization did not use the fake identity exclusively")
	}
	for _, endpoint := range []string{"https://example.invalid/openai/v1/embeddings", "http://127.0.0.1:8124/openai/v1/embeddings", "http://127.0.0.1:8123/other"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := source.Authorize(req); err == nil || req.Header.Get("Authorization") != "" {
			t.Errorf("demo identity authorized an unrelated endpoint: %s", endpoint)
		}
	}
	if _, err := source.Refresh(t.Context(), "https://example.invalid/openai/v1"); err == nil {
		t.Error("demo refresh accepted an external endpoint override")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.Refresh(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled refresh returned %v", err)
	}
}

func TestDemoFoundryRequiresLoopback(t *testing.T) {
	for _, endpoint := range []string{
		"https://example.invalid", "http://192.0.2.1:8123", "http://localhost:8123",
		"http://user:password@127.0.0.1:8123", "http://127.0.0.1:8123/other",
		"http://127.0.0.1:8123?override=1", "http://127.0.0.1:8123#other",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := newDemoFoundrySource(endpoint); err == nil {
				t.Fatal("accepted a backend that is not an unambiguous HTTP loopback base URL")
			}
		})
	}
}

func TestSetupFoundry(t *testing.T) {
	t.Setenv("AZURE_RESOURCE_ID", "ignored-external-resource")
	t.Setenv("AZURE_ENDPOINT", "https://must-not-be-contacted.invalid/openai/v1")
	t.Setenv("AZURE_CLIENT_SECRET", "ignored-external-secret")
	t.Setenv("AZURE_API_KEY", "ignored-external-key")
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			backend := testFoundryBackend(t, lang)
			dataDir := t.TempDir()
			manual, legacy, before, err := Setup(t.Context(), dataDir, lang, backend.URL())
			if err != nil {
				t.Fatal(err)
			}
			if manual.Get().Foundry || before.Foundry || !manual.HasAPIKey() {
				t.Fatal("manual demo no longer uses its legacy configuration")
			}
			_, known, err := legacy.ActiveEmbeddingProfile(t.Context())
			if err != nil || known {
				t.Fatalf("legacy vectors were assigned a profile: known=%v err=%v", known, err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}

			cfg, store, idx, err := SetupFoundry(t.Context(), dataDir, lang, backend.URL())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() }) // Also close on a failed assertion.
			if !idx.Foundry || idx.Lang != lang || len(idx.Chats) != len(before.Chats) {
				t.Fatalf("invalid Foundry index: %+v", idx)
			}
			if !cfg.Get().Foundry || !cfg.HasChatCredentials() || cfg.HasAPIKey() {
				t.Fatal("Foundry demo must use the fake identity without an API key")
			}
			status := cfg.FoundryStatus()
			if status.ResourceID != demoResourceID || status.Catalog.Endpoint != backend.URL()+"/openai/v1" {
				t.Fatalf("demo used external connection metadata: %+v", status)
			}
			client := llm.New(cfg)
			target, err := client.ConfiguredEmbeddingProfile()
			if err != nil {
				t.Fatal(err)
			}
			active, known, err := store.ActiveEmbeddingProfile(t.Context())
			if err != nil || !known || !active.SameIdentity(target) || active.Dimensions != embeddingDim {
				t.Fatalf("invalid rebuilt profile: %+v, known=%v err=%v", active, known, err)
			}
			job, known, err := store.LatestReindex(t.Context())
			if err != nil || !known || job.Status != storage.ReindexSucceeded ||
				job.TotalChunks == 0 || job.CompletedChunks != job.TotalChunks {
				t.Fatalf("fixture did not finish a real reindex: %+v, known=%v err=%v", job, known, err)
			}
			cached, known, err := store.LoadCatalog(t.Context(), demoResourceID)
			if err != nil || !known || cached.Endpoint != status.Catalog.Endpoint {
				t.Fatalf("catalog was not persisted: known=%v err=%v", known, err)
			}
			chats, err := store.ListChats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, chat := range chats {
				if _, err := cfg.ResolveDeployment(foundry.Chat, chat.Model); err != nil {
					t.Errorf("seeded chat %d has an unusable pin: %v", chat.ID, err)
				}
				if chat.Mode == storage.ChatModeImage {
					if _, err := cfg.ResolveDeployment(foundry.Images, chat.ImageModel); err != nil {
						t.Errorf("seeded image chat %d has an unusable pin: %v", chat.ID, err)
					}
				}
			}
			if err := client.VerifyChat(t.Context()); err != nil {
				t.Fatalf("local identity chat verification: %v", err)
			}
			vectors, err := client.EmbedProfile(t.Context(), active, []string{"alpha"})
			if err != nil || len(vectors) != 1 || !slices.Equal(vectors[0], Embedding("alpha")) {
				t.Fatalf("local identity embedding returned unexpected vectors: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dataDir, IndexFile))
			if err != nil {
				t.Fatal(err)
			}
			var persisted Index
			if err := json.Unmarshal(raw, &persisted); err != nil || !persisted.Foundry {
				t.Fatalf("screenshot index is not in Foundry mode: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			_, restarted, _, err := SetupFoundry(t.Context(), dataDir, lang, backend.URL())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() }) // Also close on a failed assertion.
			latest, _, err := restarted.LatestReindex(t.Context())
			if err != nil || latest.ID != job.ID {
				t.Fatalf("unchanged fixture was needlessly reindexed: %+v err=%v", latest, err)
			}
		})
	}
}

func TestFoundryDemoEmbeddingSwitch(t *testing.T) {
	backend := testFoundryBackend(t, "en")
	cfg, store, _, err := SetupFoundry(t.Context(), t.TempDir(), "en", backend.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() }) // Also close on a failed assertion.
	original, known, err := store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known {
		t.Fatalf("missing initial profile: %v", err)
	}
	next := cfg.Get()
	next.EmbeddingDeployment = "docs-next"
	if err := cfg.Save(next); err != nil {
		t.Fatal(err)
	}
	active, _, err := store.ActiveEmbeddingProfile(t.Context())
	if err != nil || active != original {
		t.Fatalf("saving a default relabeled the index: %+v err=%v", active, err)
	}
	client := llm.New(cfg)
	target, err := client.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.StartReindex(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("deliberate demo reindex failure")
	if err := rag.Reindex(t.Context(), store, job, func(context.Context, storage.EmbeddingProfile, []string) ([][]float32, error) {
		return nil, failure
	}); !errors.Is(err, failure) {
		t.Fatalf("expected failure, got %v", err)
	}
	active, _, err = store.ActiveEmbeddingProfile(t.Context())
	if err != nil || active != original {
		t.Fatalf("failed rebuild changed the index: %+v err=%v", active, err)
	}
	job, err = store.StartReindex(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := rag.Reindex(t.Context(), store, job, client.EmbedProfile); err != nil {
		t.Fatal(err)
	}
	active, known, err = store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known || !active.SameIdentity(target) || active.Dimensions != embeddingDim {
		t.Fatalf("local embedding switch did not complete: %+v known=%v err=%v", active, known, err)
	}
}

func TestBackendV1Routes(t *testing.T) {
	backend := testFoundryBackend(t, "en")
	source, err := newDemoFoundrySource(backend.URL())
	if err != nil {
		t.Fatal(err)
	}
	var edit bytes.Buffer
	writer := multipart.NewWriter(&edit)
	for name, value := range map[string]string{"model": "canvas", "prompt": "edit the demo", "output_format": "png"} {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, contentType, body, expected string
	}{
		{"/chat/completions", "application/json", `{"model":"chat-primary","messages":[{"role":"user","content":"hi"}]}`, "chat"},
		{"/chat/completions", "application/json", `{"model":"chat-primary","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "stream"},
		{"/embeddings", "application/json", `{"model":"docs-primary","input":["alpha","beta"]}`, "embeddings"},
		{"/images/generations", "application/json", `{"model":"canvas","prompt":"draw the demo","output_format":"png"}`, "image"},
		{"/images/edits", writer.FormDataContentType(), edit.String(), "image"},
	} {
		t.Run(tc.path+"/"+tc.expected, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.endpoint+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tc.contentType)
			if err := source.Authorize(req); err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }() // The response is consumed below.
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, raw)
			}
			if tc.expected == "stream" {
				if !bytes.Contains(raw, []byte("data: [DONE]")) || !bytes.Contains(raw, []byte(`"usage"`)) {
					t.Fatal("v1 stream did not complete with usage")
				}
				return
			}
			var out struct {
				Model string `json:"model"`
				Data  []struct {
					Embedding []float32 `json:"embedding"`
					Image     string    `json:"b64_json"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			switch tc.expected {
			case "chat":
				if out.Model != "chat-primary" {
					t.Errorf("response model %q lost the deployment alias", out.Model)
				}
			case "embeddings":
				if len(out.Data) != 2 || !slices.Equal(out.Data[0].Embedding, Embedding("alpha")) {
					t.Error("v1 embeddings do not match the seeded vectors")
				}
			case "image":
				if len(out.Data) != 1 {
					t.Fatalf("got %d images", len(out.Data))
				}
				image, err := base64.StdEncoding.DecodeString(out.Data[0].Image)
				if err != nil || len(image) == 0 {
					t.Errorf("invalid demo image: %v", err)
				}
			}
		})
	}
}

func testFoundryBackend(t *testing.T, lang string) *Backend {
	t.Helper()
	backend, err := StartBackend(lang)
	if err != nil {
		t.Fatal(err)
	}
	backend.streamDelay = 0
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("close demo backend: %v", err)
		}
	})
	return backend
}
