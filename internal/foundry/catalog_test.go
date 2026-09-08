package foundry

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDeploymentOperationsUseCanonicalMetadata(t *testing.T) {
	for _, test := range []struct {
		name, model, format, version, sku, state string
		capabilities                             map[string]string
		want                                     []Operation
	}{
		{name: "customer-production", model: "gpt-4o", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "vectors", model: "text-embedding-3-large", format: "OpenAI", want: []Operation{Embeddings}},
		{name: "legacy-image-format", model: "dall-e-3", format: "OpenAI"},
		{name: "editor", model: "gpt-image-1", format: "OpenAI", want: []Operation{Images, ImageEdits}},
		{name: "current-editor", model: "gpt-image-2", format: "OpenAI", want: []Operation{Images, ImageEdits}},
		{name: "current-chat", model: "gpt-5.5", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "sol", model: "gpt-5.6-sol", version: "2026-07-09", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "terra", model: "gpt-5.6-terra", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "luna", model: "gpt-5.6-luna", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "astra", model: "gpt-6-astra", version: "2026-09-03", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "instant", model: "gpt-chat-latest", version: "2026-06-24", format: "OpenAI", want: []Operation{Chat}},
		{name: "new-grok", model: "grok-4.3", format: "xAI", want: []Operation{Chat}},
		{name: "router", model: "model-router", format: "OpenAI", want: []Operation{Chat}},
		{name: "vision-router", model: "model-router", format: "OpenAI", capabilities: map[string]string{"imageInput": "true"}, want: []Operation{Chat, Vision}},
		{name: "gpt-4o", model: "whisper", format: "OpenAI"},
		{name: "advertised-chat", model: "future-model", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true"}, want: []Operation{Chat}},
		{name: "advertised-vision", model: "future-vision-model", format: "OpenAI", capabilities: map[string]string{"chat_completions": "true", "image_input": "true"}, want: []Operation{Chat, Vision}},
		{name: "advertised-grok", model: "grok-future", format: "xAI", capabilities: map[string]string{"chatCompletion": "true"}, want: []Operation{Chat}},
		{name: "missing-capabilities", model: "future-model", format: "OpenAI"},
		{name: "unknown-vision-without-chat", model: "future-model", format: "OpenAI", capabilities: map[string]string{"vision": "true"}},
		{name: "unknown-embedding", model: "future-vector-model", format: "OpenAI", capabilities: map[string]string{"embeddings": "true"}},
		{name: "unknown-format-with-hint", model: "future-model", format: "Unknown", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "unknown-disabled", model: "future-model", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "false"}},
		{name: "unknown-invalid-hint", model: "future-model", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "enabled"}},
		{name: "unknown-conflicting-hints", model: "future-model", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true", "chat_completions": "false"}},
		{name: "unknown-batch-with-hint", model: "future-model", format: "OpenAI", sku: "GlobalBatch", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "unknown-native-with-hint", model: "future-model", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true", "protocol": "native"}},
		{name: "unknown-format", model: "gpt-4o", format: "Unknown"},
		{name: "unknown-gpt", model: "gpt-new-experimental", format: "OpenAI"},
		{name: "audio", model: "gpt-4o-audio-preview", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "audio-version", model: "gpt-4o", version: "audio-preview", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "realtime", model: "gpt-realtime", format: "OpenAI"},
		{name: "responses-only", model: "gpt-5-pro", format: "OpenAI"},
		{name: "responses-only-with-hint", model: "gpt-5.4-pro", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "codex-with-hint", model: "gpt-5.3-codex", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "legacy-completion", model: "gpt-35-turbo-instruct", format: "OpenAI"},
		{name: "anthropic", model: "claude-sonnet-4-5", format: "Anthropic", capabilities: map[string]string{"chatCompletion": "true"}},
		{name: "native", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"inferenceProtocol": "anthropic-messages"}},
		{name: "batch", model: "gpt-4o", format: "OpenAI", sku: "GlobalBatch"},
		{name: "data-zone-batch", model: "gpt-4o", format: "OpenAI", sku: "DataZoneBatch"},
		{name: "batch-capability", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"batchOnly": "true"}},
		{name: "batch-capable-not-only", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"batch": "true"}, want: []Operation{Chat, Vision}},
		{name: "creating", model: "gpt-4o", format: "OpenAI", state: "Creating"},
		{name: "failed", model: "gpt-4o", format: "OpenAI", state: "Failed"},
		{name: "disabled-chat", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "false"}},
		{name: "unrecognized-value", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "enabled"}},
		{name: "conflicting-hints", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"chatCompletion": "true", "chat_completions": "false"}},
		{name: "disabled-vision", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"imageInput": "false"}, want: []Operation{Chat}},
		{name: "unsupported-image-adapter", model: "dall-e-3", format: "OpenAI", capabilities: map[string]string{"imageEdits": "true"}},
		{name: "edits-disabled", model: "gpt-image-1", format: "OpenAI", capabilities: map[string]string{"imageEditing": "false"}, want: []Operation{Images}},
		{name: "unknown-hint", model: "gpt-4o", format: "OpenAI", capabilities: map[string]string{"aFutureFeature": "true"}, want: []Operation{Chat, Vision}},
		{name: "gpt4-text-version", model: "gpt-4", version: "0613", format: "OpenAI", want: []Operation{Chat}},
		{name: "gpt4-vision-version", model: "gpt-4", version: "vision-preview", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "gpt4-turbo-version", model: "gpt-4", version: "turbo-2024-04-09", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "dated-canonical", model: "gpt-4o-2024-08-06", format: "OpenAI", want: []Operation{Chat, Vision}},
		{name: "deepseek-alias", model: "DeepSeek-R1-0528", format: "DeepSeek", want: []Operation{Chat}},
		{name: "mistral-alias", model: "Mistral-large-2411", format: "Mistral AI", want: []Operation{Chat}},
		{name: "meta-alias", model: "Llama-3.3-70B-Instruct", format: "Meta", want: []Operation{Chat}},
		{name: "meta-vision", model: "Llama-3.2-90B-Vision-Instruct", format: "Meta", want: []Operation{Chat, Vision}},
		{name: "phi-alias", model: "Phi-4-mini-instruct", format: "Microsoft", want: []Operation{Chat}},
		{name: "cohere-command", model: "Cohere-command-r-plus", format: "Cohere", want: []Operation{Chat}},
		{name: "cohere-embed-adapter", model: "Cohere-embed-v3-multilingual", format: "Cohere", capabilities: map[string]string{"embeddings": "true"}},
		{name: "cohere-rerank", model: "Cohere-rerank-v3.5", format: "Cohere"},
		{name: "grok-alias", model: "grok-3", format: "xAI", want: []Operation{Chat}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := test.state
			if state == "" {
				state = "Succeeded"
			}
			deployment := Deployment{
				Name: test.name, ModelName: test.model, ModelFormat: test.format,
				ModelVersion: test.version, ProvisioningState: state, SKU: test.sku, Capabilities: test.capabilities,
			}
			want := make(map[Operation]bool)
			for _, op := range test.want {
				want[op] = true
			}
			for _, op := range []Operation{Chat, Embeddings, Images, ImageEdits, Vision, "unknown"} {
				if got := deployment.Supports(op); got != want[op] {
					t.Errorf("Supports(%s) = %v, want %v: %s", op, got, want[op], deployment.UnsupportedReason(op))
				}
				if (deployment.UnsupportedReason(op) == "") != want[op] {
					t.Errorf("UnsupportedReason(%s) inconsistent with support", op)
				}
			}
		})
	}
	if (Deployment{ModelName: "gpt-4o", ModelFormat: "OpenAI"}).Supports(Chat) {
		t.Fatal("missing provisioning state must not imply readiness")
	}
}

func TestSnapshotHelpersAndJSON(t *testing.T) {
	snapshot := Snapshot{
		ResourceID: testResourceID, Endpoint: "https://resource.openai.azure.com/openai/v1",
		RefreshedAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
		Deployments: []Deployment{
			{ID: testResourceID + "/deployments/z-chat", Name: "z-chat", ModelName: "gpt-4o", ModelVersion: "2024-08-06", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "GlobalStandard", Capabilities: map[string]string{"chatCompletion": "true"}},
			{Name: "unknown", ModelName: "future-model", ModelFormat: "OpenAI", ProvisioningState: "Succeeded"},
			{Name: "a-chat", ModelName: "gpt-4.1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded"},
		},
	}
	if got := snapshot.Names(Chat); !reflect.DeepEqual(got, []string{"a-chat", "z-chat"}) {
		t.Fatalf("Names(chat) = %v", got)
	}
	if got := snapshot.Names(Embeddings); len(got) != 0 || got == nil {
		t.Fatalf("Names(embeddings) = %v", got)
	}
	if deployment, found := snapshot.Find("unknown"); !found || deployment.Supports(Chat) {
		t.Fatal("unsupported models must remain visible")
	}
	if _, found := snapshot.Find("missing"); found {
		t.Fatal("missing deployment found")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"resource_id", "endpoint", "deployments", "refreshed_at", "id", "name", "model_name", "model_version", "model_format", "provisioning_state", "sku", "capabilities"} {
		if !strings.Contains(string(data), `"`+key+`":`) {
			t.Errorf("JSON key %s missing", key)
		}
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil || !reflect.DeepEqual(snapshot, restored) {
		t.Fatalf("snapshot metadata did not survive JSON roundtrip: %v", err)
	}
}
