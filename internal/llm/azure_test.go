package llm

import (
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
)

// TestChatCompletionsURL checks the schema detection (classic vs. v1) for the
// chat completions URL.
func TestChatCompletionsURL(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		deployment string
		apiVersion string
		want       string
	}{
		{
			name:       "v1",
			endpoint:   "https://x.services.ai.azure.com/openai/v1",
			deployment: "model-router",
			apiVersion: "2025-01-01-preview",
			want:       "https://x.services.ai.azure.com/openai/v1/chat/completions",
		},
		{
			name:       "v1 with trailing slash",
			endpoint:   "https://x.services.ai.azure.com/openai/v1/",
			deployment: "model-router",
			apiVersion: "preview",
			want:       "https://x.services.ai.azure.com/openai/v1/chat/completions",
		},
		{
			name:       "classic",
			endpoint:   "https://x.openai.azure.com",
			deployment: "gpt-4o",
			apiVersion: "2024-08-01-preview",
			want:       "https://x.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-08-01-preview",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatCompletionsURL(tc.endpoint, tc.deployment, tc.apiVersion); got != tc.want {
				t.Errorf("chatCompletionsURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmbeddingsURL checks the embeddings URL per schema.
func TestEmbeddingsURL(t *testing.T) {
	if got := embeddingsURL("https://x.services.ai.azure.com/openai/v1", "text-embedding-3-large", "2024-02-01"); got != "https://x.services.ai.azure.com/openai/v1/embeddings" {
		t.Errorf("embeddingsURL v1 is wrong: %q", got)
	}
	if got := embeddingsURL("https://x.cognitiveservices.azure.com", "text-embedding-3-large", "2024-02-01"); got != "https://x.cognitiveservices.azure.com/openai/deployments/text-embedding-3-large/embeddings?api-version=2024-02-01" {
		t.Errorf("embeddingsURL classic is wrong: %q", got)
	}
}

// TestAPIVersionFor keeps dated versions on the classic schema and switches the
// v1 surface to the preview moniker, which is the one that exposes the full
// model catalog.
func TestAPIVersionFor(t *testing.T) {
	if got := apiVersionFor("https://x.services.ai.azure.com/openai/v1", "2024-08-01-preview"); got != "preview" {
		t.Errorf("v1 with a dated version: got %q, want preview", got)
	}
	if got := apiVersionFor("https://x.services.ai.azure.com/openai/v1", "v1"); got != "v1" {
		t.Errorf("an explicit v1 moniker must be kept: %q", got)
	}
	if got := apiVersionFor("https://x.openai.azure.com", "2024-08-01-preview"); got != "2024-08-01-preview" {
		t.Errorf("classic must keep the configured version: %q", got)
	}
}

// TestChatModelField verifies that with the v1 schema the deployment is used as
// the model, that a pinned model takes precedence and that the classic schema
// leaves the field empty (router decides).
func TestChatModelField(t *testing.T) {
	v1 := config.Config{Endpoint: "https://x.services.ai.azure.com/openai/v1", ChatDeployment: "model-router"}
	if got := chatModelField(v1, ""); got != "model-router" {
		t.Errorf("v1 without a pinned model: got %q, want model-router", got)
	}
	v1.ChatModel = "gpt-4o"
	if got := chatModelField(v1, ""); got != "gpt-4o" {
		t.Errorf("v1 with a pinned model: got %q, want gpt-4o", got)
	}
	if got := chatModelField(v1, "o3"); got != "o3" {
		t.Errorf("the per-chat model must win: got %q, want o3", got)
	}
	classic := config.Config{Endpoint: "https://x.openai.azure.com", ChatDeployment: "model-router"}
	if got := chatModelField(classic, ""); got != "" {
		t.Errorf("classic without a pinned model: got %q, want empty", got)
	}
}

// TestChatDeployment covers the routing of a picked model: on the classic schema
// it has to replace the deployment in the URL path, because there the path - not
// the body - decides which deployment answers.
func TestChatDeployment(t *testing.T) {
	classic := config.Config{Endpoint: "https://x.openai.azure.com", ChatDeployment: "model-router"}
	if got := chatDeployment(classic, ""); got != "model-router" {
		t.Errorf("classic without a selection: got %q, want model-router", got)
	}
	if got := chatDeployment(classic, "o4-mini"); got != "o4-mini" {
		t.Errorf("classic with a selection: got %q, want o4-mini", got)
	}
	classic.ChatModel = "gpt-4.1"
	if got := chatDeployment(classic, ""); got != "gpt-4.1" {
		t.Errorf("classic with a pinned model: got %q, want gpt-4.1", got)
	}

	v1 := config.Config{Endpoint: "https://x.services.ai.azure.com/openai/v1", ChatDeployment: "model-router"}
	if got := chatDeployment(v1, "o4-mini"); got != "o4-mini" {
		t.Errorf("v1 with a selection: got %q, want o4-mini", got)
	}
}
