package foundry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestImageModelsUseInferenceCatalog(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" || r.URL.Query().Get("api-version") != "preview" ||
			r.Header.Get("Authorization") != "Bearer offline-inference-token" {
			t.Errorf("unexpected model catalog request: %s", r.URL)
		}
		writeJSON(t, w, map[string]any{"data": []map[string]any{
			{"id": "gpt-image-1-2025-04-15", "owned_by": nil},
			{"id": "gpt-image-1-mini-2025-10-06"},
			{"id": "gpt-image-1.5-2025-12-16"},
			{"id": "gpt-image-2-2026-04-21"},
			{"id": "gpt-image-1"},
			{"id": "gpt-image-1-mini"},
			{"id": "gpt-image-1.5"},
			{"id": "gpt-image-2"},
			{"id": "gpt-image-2"},
			{"id": "gpt-6-astra"},
			{"id": "future-image-model"},
		}})
	}))
	defer server.Close()
	credential := &fakeCredential{}
	client := newClient(testResourceID, credential)
	client.httpClient = server.Client()
	models, err := client.ImageModels(t.Context(), server.URL+"/openai/v1")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, model := range models {
		names = append(names, model.Name)
		if model.Source != ModelsAPISource || model.ProvisioningState != "" || !model.Supports(Images) ||
			!model.Supports(ImageEdits) || model.Supports(Chat) || model.Supports(Embeddings) || model.Supports(Vision) {
			t.Fatalf("API model provenance or operations are incorrect: %+v", model)
		}
	}
	if !slices.Equal(names, []string{"gpt-image-1", "gpt-image-1-mini", "gpt-image-1.5", "gpt-image-2"}) {
		t.Fatalf("image models = %v", names)
	}
	if !slices.Equal(credential.requestedScopes(), []string{inferenceScope}) {
		t.Fatal("model catalog did not use the inference audience")
	}
}

func TestImageModelsPreserveVersionOnlyEntries(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"data": []map[string]string{{"id": "gpt-image-2-2026-04-21"}}})
	}))
	defer server.Close()
	client := newClient(testResourceID, &fakeCredential{})
	client.httpClient = server.Client()
	models, err := client.ImageModels(t.Context(), server.URL+"/openai/v1")
	if err != nil || len(models) != 1 || models[0].ModelVersion != "2026-04-21" ||
		models[0].Name != "gpt-image-2-2026-04-21" {
		t.Fatalf("versioned model = %+v, %v", models, err)
	}
}

func TestImageModelsRejectIncompleteOrDeniedCatalog(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"forbidden", http.StatusForbidden, `{"error":"denied"}`},
		{"missing data", http.StatusOK, `{}`},
		{"incomplete", http.StatusOK, `{"data":[],"has_more":true}`},
		{"unexpected pagination", http.StatusOK, `{"data":[],"nextLink":"https://other.example"}`},
		{"malformed", http.StatusOK, `broken`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body)) // Local fixture response only.
			}))
			defer server.Close()
			client := newClient(testResourceID, &fakeCredential{})
			client.httpClient = server.Client()
			if models, err := client.ImageModels(context.Background(), server.URL+"/openai/v1"); err == nil || models != nil {
				t.Fatalf("incomplete catalog was accepted: %+v, %v", models, err)
			}
		})
	}
}
