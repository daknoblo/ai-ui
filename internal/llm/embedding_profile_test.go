package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestManualEmbeddingProfileAuthorizationAndRestart(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("api-key") != "key" {
			t.Error("missing embedding credential")
		}
		if !strings.Contains(r.URL.Path, "/old-vectors/") || r.URL.Query().Get("api-version") != "old-version" {
			t.Errorf("old profile transport changed: %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`)) // Test response only.
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	store := config.NewStore(path, config.Keys{Embedding: "key"}, config.Overrides{})
	cfg := store.Get()
	cfg.EmbeddingEndpoint, cfg.EmbeddingDeployment, cfg.EmbeddingAPIVersion = upstream.URL, "old-vectors", "old-version"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	client := New(store)
	profile, err := client.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}
	profile.Dimensions = 2
	cfg.EmbeddingEndpoint = "https://new.example/openai/v1"
	cfg.EmbeddingDeployment, cfg.EmbeddingAPIVersion = "new-vectors", "new-version"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedProfile(t.Context(), profile, []string{"old index query"}); err != nil {
		t.Fatal(err)
	}
	untrusted := profile
	untrusted.Endpoint = "https://unconfigured.example/openai/v1"
	if _, err := client.EmbedProfile(t.Context(), untrusted, []string{"query"}); err == nil {
		t.Fatal("profile authorized an unconfigured credential destination")
	}
	restarted := config.NewStore(path, config.Keys{Embedding: "different-key"}, config.Overrides{})
	if _, err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(restarted).EmbedProfile(t.Context(), profile, []string{"query"}); err == nil || calls.Load() != 1 {
		t.Fatal("restart sent new credentials to an old, incompatible profile target")
	}
}

func TestEmbeddingProfileAuthTransitionsRequireExplicitRebuild(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if (r.Header.Get("api-key") == "") == (r.Header.Get("Authorization") == "") {
			t.Error("expected exactly one authentication mechanism")
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`)) // Test response only.
	}))
	defer upstream.Close()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{API: "key"}, config.Overrides{})
	cfg := store.Get()
	cfg.Endpoint, cfg.EmbeddingDeployment = upstream.URL+"/openai/v1", "vectors"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	client := New(store)
	manual, err := client.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}
	store.ConfigureFoundry(llmResourceID, llmIdentity{}, nil)
	if err := store.SetCatalog(foundry.Snapshot{
		ResourceID: llmResourceID, Endpoint: cfg.Endpoint,
		Deployments: []foundry.Deployment{{Name: "vectors", ModelName: "text-embedding-3-small", ModelVersion: "1",
			ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedProfile(t.Context(), manual, []string{"query"}); err == nil || calls.Load() != 0 {
		t.Fatal("manual index silently adapted to Foundry identity")
	}
	discovered, err := client.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}
	if discovered.SameIdentity(manual) {
		t.Fatal("different authentication identity must require reindexing")
	}
	if _, err := client.EmbedProfile(t.Context(), discovered, []string{"explicit rebuild"}); err != nil {
		t.Fatal(err)
	}
	store.ConfigureFoundry("", nil, nil)
	if _, err := client.EmbedProfile(t.Context(), discovered, []string{"query"}); err == nil || calls.Load() != 1 {
		t.Fatal("Foundry index silently adapted to an API key")
	}
	if _, err := client.EmbedProfile(t.Context(), manual, []string{"explicit rebuild"}); err != nil {
		t.Fatal(err)
	}
}

func TestManualEmbeddingProfilesExcludeURLSecretsAndFreezeDimensions(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{API: "key"}, config.Overrides{})
	cfg := store.Get()
	cfg.Endpoint, cfg.EmbeddingDeployment = "https://user:secret@example.com/openai/v1", "vectors"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	client := New(store)
	if _, err := client.ConfiguredEmbeddingProfile(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("credential-bearing endpoint must be rejected without echoing credentials")
	}
	var profile storage.EmbeddingProfile
	if err := json.Unmarshal([]byte(`{"endpoint":"https://example.com/openai/v1","deployment":"vectors","dimensions":2}`), &profile); err != nil {
		t.Fatal(err)
	}
	cfg.Endpoint = profile.Endpoint
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	current, err := client.ConfiguredEmbeddingProfile()
	if err != nil || !current.SameIdentity(profile) {
		t.Fatalf("manual identity must persist independently of dimensions: %+v, %v", current, err)
	}
}

func TestLegacyClassicEmbeddingProfileRequiresPinnedVersion(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{API: "key"}, config.Overrides{})
	cfg := store.Get()
	cfg.Endpoint, cfg.EmbeddingDeployment, cfg.APIVersion = upstream.URL, "vectors", "new-version"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	profile := storage.EmbeddingProfile{Endpoint: upstream.URL, Deployment: "vectors", Dimensions: 2}
	_, err := New(store).EmbedProfile(t.Context(), profile, []string{"query"})
	if err == nil || !strings.Contains(err.Error(), "no pinned API version") {
		t.Fatalf("legacy classic profile must explicitly require a rebuild, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("legacy profile sent a request without its original API version")
	}
}
