package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/llm"
)

type replicaIdentity struct{}

func (replicaIdentity) Refresh(context.Context, string) (foundry.Snapshot, error) {
	return foundry.Snapshot{}, fmt.Errorf("offline fixture")
}
func (replicaIdentity) ImageModels(context.Context, string) ([]foundry.Deployment, error) {
	return nil, nil
}
func (replicaIdentity) Authorize(r *http.Request) error {
	r.Header.Set("Authorization", "Bearer fixture")
	return nil
}

func TestReplicaVectorSpaceAcrossIngestQueryAndAtomicReindex(t *testing.T) {
	fixture := newReindexFixture(t, 0, false)
	var mu sync.Mutex
	var providers []string
	var badDimensions atomic.Bool
	handler := func(provider string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/openai/v1/embeddings" || r.Header.Get("api-key") != "" ||
				r.Header.Get("Authorization") != "Bearer fixture" {
				t.Error("embedding destination or authorization mismatch")
			}
			var body struct {
				Model string
				Input []string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "vectors" {
				t.Errorf("wrong embedding operation: %v, %+v", err, body)
			}
			mu.Lock()
			providers = append(providers, provider)
			mu.Unlock()
			data := make([]map[string]any, len(body.Input))
			for i := range data {
				vector := []float32{0.5, 0.5}
				if badDimensions.Load() && provider == "B" {
					vector = append(vector, 0.1)
				}
				data[i] = map[string]any{"index": i, "embedding": vector}
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
				t.Error(err)
			}
		}
	}
	a, b := httptest.NewServer(handler("A")), httptest.NewServer(handler("B"))
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	resource := "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/a"
	deployment := foundry.Deployment{Name: "vectors", ModelName: "text-embedding-3-small", ModelVersion: "1",
		ModelFormat: "OpenAI", ProvisioningState: "Succeeded", EmbeddingDimensions: 2}
	root := foundry.Snapshot{ResourceID: resource, Endpoint: a.URL + "/openai/v1", Deployments: []foundry.Deployment{deployment}}
	replica := root
	replica.ResourceID += "-b"
	replica.Endpoint = b.URL + "/openai/v1"
	root.Accounts = []foundry.Snapshot{replica}
	cfg := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{}, config.Overrides{})
	if _, err := cfg.Load(); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigureFoundry(resource, replicaIdentity{}, nil)
	if err := cfg.SetCatalog(root); err != nil {
		t.Fatal(err)
	}
	settings := cfg.Get()
	settings.EnabledDeployments = map[foundry.Operation][]string{foundry.Embeddings: {root.Key(deployment), replica.Key(deployment)}}
	if err := cfg.Save(settings); err != nil {
		t.Fatal(err)
	}
	client := llm.New(cfg)
	profile, err := client.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}
	legacy := profile
	legacy.VectorSpace = ""
	if err := fixture.store.SetInitialEmbeddingProfile(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("Replica-compatible document text. ", 1300)
	if _, err := NewIngestor(fixture.store, client, Prompts{}).Ingest(t.Context(), fixture.chatID, "vectors.txt", "text/plain", []byte(text)); err != nil {
		t.Fatal(err)
	}
	hits, err := NewRetriever(fixture.store, client).Retrieve(t.Context(), fixture.chatID, "document text", 3)
	if err != nil || len(hits) == 0 {
		t.Fatalf("replica query failed: %v", err)
	}
	mu.Lock()
	if len(providers) < 3 || providers[0] != "A" || providers[1] != "B" || providers[2] != "A" {
		t.Fatalf("ingest/query did not cycle compatible replicas: %v", providers)
	}
	mu.Unlock()
	job, err := fixture.store.StartReindex(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reindex(t.Context(), fixture.store, job, client.EmbedProfile); err != nil {
		t.Fatal(err)
	}
	active, known, err := fixture.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known || !active.SameIdentity(legacy) {
		t.Fatalf("replica reindex lost identity: %+v, %v", active, err)
	}
	before, err := reindexVectors(t.Context(), fixture.store, fixture.chatID)
	if err != nil {
		t.Fatal(err)
	}
	badDimensions.Store(true)
	job, err = fixture.store.StartReindex(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reindex(t.Context(), fixture.store, job, client.EmbedProfile); err == nil {
		t.Fatal("mismatched dimensions were committed")
	}
	after, err := reindexVectors(t.Context(), fixture.store, fixture.chatID)
	current, _, profileErr := fixture.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || profileErr != nil || !reflect.DeepEqual(before, after) || current != active {
		t.Fatal("failed replica reindex changed active vectors/profile")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(providers, "A") || !slices.Contains(providers, "B") {
		t.Fatal("reindex did not use the replica pool")
	}
}
