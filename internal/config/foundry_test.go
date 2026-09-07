package config

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

type testFoundrySource struct{}

func (testFoundrySource) Refresh(context.Context, string) (foundry.Snapshot, error) {
	return foundry.Snapshot{}, errors.New("offline")
}

func (testFoundrySource) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer test-token")
	return nil
}

const testResource = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/test"

func testCatalog() foundry.Snapshot {
	return foundry.Snapshot{
		ResourceID: testResource, Endpoint: "https://test.services.ai.azure.com/openai/v1",
		RefreshedAt: time.Now(),
		Deployments: []foundry.Deployment{
			{Name: "my-chat", ModelName: "gpt-4o", ModelVersion: "2024-08-06", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "GlobalStandard"},
			{Name: "my-vectors", ModelName: "text-embedding-3-small", ModelVersion: "1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
		},
	}
}

func newFoundryStore(t *testing.T, overrides Overrides) *Store {
	t.Helper()
	s := NewStore(filepath.Join(t.TempDir(), "config.json"), Keys{API: "legacy-secret"}, overrides)
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	s.ConfigureFoundry(testResource, testFoundrySource{}, nil)
	if err := s.SetCatalog(testCatalog()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFoundrySelectionsUseCatalog(t *testing.T) {
	s := newFoundryStore(t, Overrides{})
	cfg := s.Get()
	if !slices.Equal(cfg.ChatModels, []string{"my-chat"}) {
		t.Fatalf("chat models = %v", cfg.ChatModels)
	}
	cfg.ChatDeployment = "my-chat"
	cfg.EmbeddingDeployment = "my-vectors"
	cfg.APIVersion = ""
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if !s.IsConfigured() {
		t.Fatal("v1 with an identity and selected deployment must be configured without a dated version")
	}
	if err := s.SetChatModel("my-chat"); err != nil {
		t.Fatal(err)
	}
	if s.Get().ChatModel != "my-chat" {
		t.Fatal("catalog selection was cleared because AZURE_MODELS is absent")
	}
	if err := s.SetChatModel("my-vectors"); err == nil {
		t.Fatal("embedding deployment accepted as chat")
	}
	cfg.ChatDeployment = "my-vectors"
	if err := s.Save(cfg); err == nil {
		t.Fatal("invalid role was persisted")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"legacy-secret", "test-token", "Authorization"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("configuration contains a credential: %s", secret)
		}
	}
}

func TestFoundryRefreshPreservesMissingSelection(t *testing.T) {
	s := newFoundryStore(t, Overrides{})
	cfg := s.Get()
	cfg.ChatDeployment = "my-chat"
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChatModel("my-chat"); err != nil {
		t.Fatal(err)
	}
	snapshot := testCatalog()
	snapshot.Deployments = nil
	if err := s.SetCatalog(snapshot); err != nil {
		t.Fatal(err)
	}
	if s.Get().ChatDeployment != "my-chat" || s.Get().ChatModel != "my-chat" {
		t.Fatal("refresh discarded a saved selection")
	}
	if s.IsConfigured() {
		t.Fatal("missing deployment must not be considered usable")
	}
	s.SetDiscoveryError(errors.New("permission denied"))
	if s.FoundryStatus().RefreshError == "" || s.Get().ChatDeployment != "my-chat" {
		t.Fatal("refresh failure must be visible without clearing choices")
	}
}

func TestFoundryAuthorizationAndResourceBoundary(t *testing.T) {
	s := newFoundryStore(t, Overrides{})
	for _, tc := range []struct {
		url   string
		op    foundry.Operation
		model string
		ok    bool
	}{
		{"https://test.services.ai.azure.com/openai/v1/chat/completions", foundry.Chat, "my-chat", true},
		{"https://test.services.ai.azure.com/openai/v1/embeddings", foundry.Embeddings, "my-vectors", true},
		{"https://other.services.ai.azure.com/openai/v1/chat/completions", foundry.Chat, "my-chat", false},
		{"https://test.services.ai.azure.com/another-api", foundry.Chat, "my-chat", false},
		{"https://test.services.ai.azure.com/openai/v1/chat/completions", foundry.Chat, "my-vectors", false},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Authorize(req, tc.op, tc.model)
		if (err == nil) != tc.ok {
			t.Errorf("%s / %s: %v", tc.url, tc.model, err)
		}
		if tc.ok && (req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("api-key") != "") {
			t.Error("identity request used the wrong authentication")
		}
		if !tc.ok && req.Header.Get("Authorization") != "" {
			t.Error("credential attached to an invalid destination")
		}
	}
}

func TestFoundryCredentialFailureDoesNotFallBack(t *testing.T) {
	s := newFoundryStore(t, Overrides{})
	s.ConfigureFoundry(testResource, nil, errors.New("missing client secret"))
	if s.HasChatCredentials() || s.HasEmbeddingCredentials() || s.HasImageCredentials() {
		t.Fatal("identity mode fell back to an available legacy key")
	}
	if s.FoundryStatus().IdentityError == "" {
		t.Fatal("identity error is hidden")
	}
}

func TestFoundrySnapshotIsolationAndEnvFilter(t *testing.T) {
	s := newFoundryStore(t, Overrides{ChatModels: []string{"my-chat", "missing"}})
	if !slices.Equal(s.Get().ChatModels, []string{"my-chat"}) {
		t.Fatal("environment filter added an undiscovered deployment")
	}
	snapshot := testCatalog()
	snapshot.Deployments[0].Capabilities = map[string]string{"chatCompletion": "true"}
	if err := s.SetCatalog(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Deployments[0].Name = "mutated"
	copy := s.FoundryStatus()
	copy.Catalog.Deployments[0].Capabilities["chatCompletion"] = "false"
	if _, err := s.ResolveDeployment(foundry.Chat, "my-chat"); err != nil {
		t.Fatalf("external mutation changed the stored inventory: %v", err)
	}
}

func TestFoundrySettingsSurviveRestart(t *testing.T) {
	store := newFoundryStore(t, Overrides{})
	cfg := store.Get()
	cfg.ChatDeployment, cfg.ChatModel = "my-chat", "my-chat"
	cfg.EmbeddingDeployment, cfg.VisionDeployment = "my-vectors", "my-chat"
	cfg.APIVersion = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(store.path, Keys{}, Overrides{})
	if _, err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	restarted.ConfigureFoundry(testResource, testFoundrySource{}, nil)
	if err := restarted.SetCatalog(testCatalog()); err != nil {
		t.Fatal(err)
	}
	actual := restarted.Get()
	if actual.ChatDeployment != cfg.ChatDeployment || actual.ChatModel != cfg.ChatModel ||
		actual.EmbeddingDeployment != cfg.EmbeddingDeployment || actual.VisionDeployment != cfg.VisionDeployment ||
		!restarted.IsConfigured() {
		t.Fatalf("saved role selections did not survive restart: %+v", actual)
	}
}
