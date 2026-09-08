package server

import (
	"net/url"
	"testing"
)

func TestManualEmbeddingSettingsLockedDuringReindex(t *testing.T) {
	for _, field := range []string{"endpoint", "embedding_endpoint", "embedding_deployment", "api_version", "embedding_api_version"} {
		t.Run(field, func(t *testing.T) {
			backend := newManualEmbeddingBackend(t, 2)
			srv, handler, chatID := newManualEmbeddingServer(t, backend)
			seedManualEmbedding(t, srv, chatID, true)
			previous := srv.cfg.Get()
			target, err := srv.llm.ConfiguredEmbeddingProfile()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := srv.store.StartReindex(t.Context(), target); err != nil {
				t.Fatal(err)
			}
			form := url.Values{
				"language": {"en"}, "endpoint": {previous.Endpoint},
				"chat_deployment":       {previous.ChatDeployment},
				"embedding_endpoint":    {previous.EmbeddingEndpoint},
				"embedding_deployment":  {previous.EmbeddingDeployment},
				"api_version":           {previous.APIVersion},
				"embedding_api_version": {previous.EmbeddingAPIVersion},
			}
			value := "different"
			if field == "endpoint" || field == "embedding_endpoint" {
				value = "https://different.example/openai/v1"
			}
			form.Set(field, value)
			postFoundryForm(handler, "/config", form)
			got := srv.cfg.Get()
			if got.EmbeddingHost() != previous.EmbeddingHost() || got.EmbeddingDeployment != previous.EmbeddingDeployment ||
				got.EmbeddingVersion() != previous.EmbeddingVersion() {
				t.Fatalf("reindex target changed through %s: %+v", field, got)
			}
		})
	}
}
