package config

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestEmbeddingAuthorizationUsesTheInferenceURLSchema(t *testing.T) {
	for _, endpoint := range []string{"https://example.com", "https://example.com/proxy/v1", "https://example.com/openai/v1"} {
		t.Run(endpoint, func(t *testing.T) {
			store := NewStore(filepath.Join(t.TempDir(), "config.json"), Keys{Embedding: "key"}, Overrides{
				EmbeddingEndpoint: endpoint, EmbeddingDeployment: "vectors",
			})
			path := endpoint + "/openai/deployments/vectors/embeddings?api-version=2024-02-01"
			if foundry.IsV1Endpoint(endpoint) {
				path = endpoint + "/embeddings"
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Authorize(req, foundry.Embeddings, "vectors"); err != nil {
				t.Fatalf("inference URL rejected: %v", err)
			}
			if req.Header.Get("api-key") != "key" {
				t.Fatal("embedding request is missing its credential")
			}
		})
	}
}
