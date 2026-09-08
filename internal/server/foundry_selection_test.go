package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestFoundryCurrentModelsAreSelectableAndSaved(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			srv, handler, source, calls := newFoundryTestServer(t, language)
			source.snapshot.Deployments = nil
			for _, model := range []struct{ name, format, version string }{
				{"gpt-5.6-sol", "OpenAI", "2026-07-09"},
				{"gpt-6-astra", "OpenAI", "2026-09-03"},
				{"gpt-chat-latest", "OpenAI", "2026-06-24"},
				{"grok-4.3", "xAI", "1"},
				{"model-router", "OpenAI", "2025-11-18"},
				{"text-embedding-3-large", "OpenAI", "1"},
			} {
				source.snapshot.Deployments = append(source.snapshot.Deployments, foundry.Deployment{
					Name: model.name, ModelName: model.name, ModelFormat: model.format,
					ModelVersion: model.version, ProvisioningState: "Succeeded", SKU: "GlobalStandard",
				})
			}
			response := postFoundryForm(handler, "/config/deployments/refresh", nil)
			if response.Code != http.StatusOK || calls.Load() != 0 {
				t.Fatalf("refresh status=%d inference=%d", response.Code, calls.Load())
			}
			view := srv.foundryData(t.Context())
			wantChats := []string{"gpt-5.6-sol", "gpt-6-astra", "gpt-chat-latest", "grok-4.3", "model-router"}
			if actual := choiceNames(view.ChatChoices); !slices.Equal(actual, wantChats) {
				t.Fatalf("chat choices = %v, want %v", actual, wantChats)
			}
			if actual := choiceNames(view.EmbeddingChoices); !slices.Equal(actual, []string{"text-embedding-3-large"}) {
				t.Fatalf("embedding choices = %v", actual)
			}
			if actual := choiceNames(view.VisionChoices); !slices.Equal(actual, []string{"gpt-5.6-sol", "gpt-6-astra"}) {
				t.Fatalf("vision choices = %v", actual)
			}
			if len(view.ImageChoices) != 0 || view.ChatFilterActive || view.ImageFilterActive {
				t.Fatal("inventory was misclassified or unexpectedly filtered")
			}
			if srv.cfg.Get().ChatDeployment != "" || srv.cfg.Get().EmbeddingDeployment != "" {
				t.Fatal("refresh must not automatically select defaults")
			}
			for _, name := range wantChats {
				if !strings.Contains(response.Body.String(), `<option value="`+name+`"`) {
					t.Errorf("rendered selector omits %s", name)
				}
				saved := postFoundryForm(handler, "/config", url.Values{
					"language": {language}, "chat_deployment": {name},
					"embedding_deployment": {"text-embedding-3-large"}, "vision_deployment": {"gpt-5.6-sol"},
				})
				cfg := srv.cfg.Get()
				if saved.Code != http.StatusOK || cfg.ChatDeployment != name ||
					cfg.ChatModel != name || cfg.EmbeddingDeployment != "text-embedding-3-large" {
					t.Fatalf("model %s was not saved: status=%d config=%+v", name, saved.Code, cfg)
				}
			}
		})
	}
}

func TestFoundryEnvironmentFiltersAreVisible(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			srv, handler, source, _ := newFoundryTestServerWithOverrides(t, language, config.Overrides{
				ChatModels: []string{"chat-prod"}, ImageModels: []string{"pictures-prod"},
			})
			source.snapshot.Deployments = append(source.snapshot.Deployments,
				foundry.Deployment{Name: "new-chat", ModelName: "gpt-6-astra", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
				foundry.Deployment{Name: "new-images", ModelName: "gpt-image-2", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			)
			response := postFoundryForm(handler, "/config/deployments/refresh", nil)
			view := srv.foundryData(t.Context())
			if !view.ChatFilterActive || !view.ImageFilterActive ||
				!slices.Equal(choiceNames(view.ChatChoices), []string{"chat-prod"}) ||
				!slices.Equal(choiceNames(view.ImageChoices), []string{"pictures-prod"}) {
				t.Fatal("explicit filters must remain enforced and be reported")
			}
			for _, key := range []string{"foundry.chat_filter_hint", "foundry.image_filter_hint"} {
				if !strings.Contains(response.Body.String(), srv.t(key)) {
					t.Errorf("missing filter warning: %s", key)
				}
			}
		})
	}
}

func choiceNames(choices []deploymentChoice) []string {
	names := make([]string, len(choices))
	for i, choice := range choices {
		names[i] = choice.Name
	}
	return names
}
