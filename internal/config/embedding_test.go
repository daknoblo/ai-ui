package config

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestNormalizeEmbeddingEndpoint(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{" https://EXAMPLE.com:443/openai/v1/// ", "https://example.com/openai/v1"},
		{"http://EXAMPLE.com:80/", "http://example.com"},
		{"https://[::1]:443/v1/", "https://[::1]/v1"},
		{"https://example.com:8443/OpenAI/v1/", "https://example.com:8443/OpenAI/v1"},
	} {
		got, err := NormalizeEmbeddingEndpoint(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("normalize %q = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"", "example.com", "ftp://example.com", "https://user:secret@example.com",
		"https://example.com?api-key=secret", "https://example.com?", "https://example.com#secret"} {
		if _, err := NormalizeEmbeddingEndpoint(raw); err == nil {
			t.Errorf("unsafe endpoint accepted: %q", raw)
		}
	}
}

func TestEmbeddingKeyEndpointBindings(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "dedicated"}[dedicated], func(t *testing.T) {
			keys := Keys{API: "chat-key"}
			if dedicated {
				keys.Embedding = "embedding-key"
			}
			path := filepath.Join(t.TempDir(), "config.json")
			store := NewStore(path, keys, Overrides{})
			if _, err := store.Load(); err != nil {
				t.Fatal(err)
			}
			cfg := store.Get()
			cfg.Endpoint = "https://FIRST.example:443/openai/v1/"
			cfg.EmbeddingDeployment = "vectors"
			if err := store.Save(cfg); err != nil {
				t.Fatal(err)
			}
			check := func(store *Store, endpoint string, allowed bool) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+"/embeddings", nil)
				if err != nil {
					t.Fatal(err)
				}
				err = store.Authorize(req, foundry.Embeddings, "vectors")
				if (err == nil) != allowed {
					t.Fatalf("authorize %s = %v, want allowed=%v", endpoint, err, allowed)
				}
				wantKey := ""
				if allowed {
					wantKey = store.EmbeddingAPIKey()
				}
				if req.Header.Get("api-key") != wantKey {
					t.Fatal("incorrect credential attached to embedding destination")
				}
			}
			check(store, "https://first.example/openai/v1", true)
			check(store, "https://unconfigured.example/openai/v1", false)
			check(store, "http://first.example/openai/v1", false)
			check(store, "https://first.example/other/v1", false)
			cfg.EmbeddingEndpoint = "https://second.example/openai/v1"
			if err := store.Save(cfg); err != nil {
				t.Fatal(err)
			}
			if store.EmbeddingKeyMismatch() == dedicated {
				t.Fatal("cross-origin key warning does not match the dedicated-key policy")
			}
			check(store, "https://second.example/openai/v1", dedicated)
			check(store, "https://first.example/openai/v1", true)
			restarted := NewStore(path, keys, Overrides{})
			if _, err := restarted.Load(); err != nil {
				t.Fatal(err)
			}
			check(restarted, "https://first.example/openai/v1", false)
			check(restarted, "https://second.example/openai/v1", dedicated)
		})
	}
}

func TestEmbeddingKeyBindingsRespectEnvironmentLocks(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "config.json"), Keys{Embedding: "key"},
		Overrides{EmbeddingEndpoint: "https://locked.example/openai/v1", EmbeddingDeployment: "locked"})
	cfg := store.Get()
	cfg.EmbeddingEndpoint = "https://untrusted.example/openai/v1"
	cfg.EmbeddingDeployment = "injected"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := store.Get(); got.EmbeddingEndpoint != "https://locked.example/openai/v1" || got.EmbeddingDeployment != "locked" {
		t.Fatalf("environment locks changed: %+v", got)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, cfg.EmbeddingEndpoint+"/embeddings", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Authorize(req, foundry.Embeddings, "injected"); err == nil || req.Header.Get("api-key") != "" {
		t.Fatal("locked field submission authorized a new credential destination")
	}
}
