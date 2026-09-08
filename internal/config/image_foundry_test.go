package config

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

const testImageResource = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/images"

type imageTestSource struct {
	snapshot   foundry.Snapshot
	refreshErr error
	refreshes  atomic.Int64
	imageReads atomic.Int64
	auth       string
	override   string
}

func (s *imageTestSource) Refresh(_ context.Context, override string) (foundry.Snapshot, error) {
	s.refreshes.Add(1)
	s.override = override
	if s.refreshErr != nil {
		return foundry.Snapshot{}, s.refreshErr
	}
	return cloneCatalog(s.snapshot), nil
}

func (s *imageTestSource) ImageModels(context.Context, string) ([]foundry.Deployment, error) {
	s.imageReads.Add(1)
	return []foundry.Deployment{{
		Name: "synthetic", ModelName: "gpt-image-2", ModelFormat: "OpenAI",
		Source: foundry.ModelsAPISource,
	}}, nil
}

func (s *imageTestSource) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", s.auth)
	return nil
}

func splitCatalog(resourceID, endpoint string) foundry.Snapshot {
	return foundry.Snapshot{
		ResourceID: resourceID, Endpoint: endpoint, RefreshedAt: time.Now(),
		Deployments: []foundry.Deployment{
			{Name: "shared", ModelName: "gpt-image-2", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "GlobalStandard"},
			{Name: "edit", ModelName: "gpt-image-1.5", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "GlobalStandard"},
		},
	}
}

func newSplitStore(t *testing.T, overrides Overrides) (*Store, *imageTestSource, *imageTestSource) {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "config.json"), Keys{API: "manual"}, overrides)
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	primary := &imageTestSource{snapshot: testCatalog(), auth: "primary"}
	primary.snapshot.Deployments = append(primary.snapshot.Deployments, foundry.Deployment{
		Name: "shared", ModelName: "gpt-4o", ModelFormat: "OpenAI",
		ProvisioningState: "Succeeded", SKU: "GlobalStandard",
	})
	images := &imageTestSource{
		snapshot: splitCatalog(testImageResource, "https://images.openai.azure.com/openai/v1"),
		auth:     "images",
	}
	store.ConfigureFoundry(testResource, primary, nil)
	store.ConfigureImageFoundry(testImageResource, images, nil)
	if err := store.SetCatalog(primary.snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.SetImageCatalog(images.snapshot); err != nil {
		t.Fatal(err)
	}
	return store, primary, images
}

func TestSeparateImageResourceUsesIndependentCatalogAndAuthorization(t *testing.T) {
	store, _, _ := newSplitStore(t, Overrides{})
	cfg := store.Get()
	if cfg.ImageHost() != "https://images.openai.azure.com/openai/v1" ||
		!slices.Equal(cfg.ImageModels, []string{"edit", "shared"}) ||
		!slices.Equal(cfg.ChatModels, []string{"my-chat", "shared"}) {
		t.Fatalf("resource choices were mixed: %+v", cfg)
	}
	image, err := store.ResolveDeployment(foundry.Images, "shared")
	if err != nil || image.ModelName != "gpt-image-2" {
		t.Fatalf("image alias resolved against primary: %+v, %v", image, err)
	}
	chat, err := store.ResolveDeployment(foundry.Chat, "shared")
	if err != nil || chat.ModelName != "gpt-4o" {
		t.Fatalf("chat alias resolved against images: %+v, %v", chat, err)
	}

	for _, tc := range []struct {
		op       foundry.Operation
		endpoint string
		wantAuth string
	}{
		{foundry.Chat, cfg.Endpoint + "/chat/completions", "primary"},
		{foundry.Images, cfg.ImageHost() + "/images/generations", "images"},
		{foundry.ImageEdits, cfg.ImageHost() + "/images/edits", "images"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Authorize(req, tc.op, "shared"); err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		if got := req.Header.Get("Authorization"); got != tc.wantAuth {
			t.Errorf("%s authorization = %q", tc.op, got)
		}
	}
	wrong, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, cfg.Endpoint+"/images/generations", nil)
	if err := store.Authorize(wrong, foundry.Images, "shared"); err == nil ||
		wrong.Header.Get("Authorization") != "" {
		t.Fatal("image credentials were attached to the primary endpoint")
	}
}

func TestSeparateImageDiscoveryIsARMOnly(t *testing.T) {
	store, primary, images := newSplitStore(t, Overrides{ImageEndpoint: "https://images.openai.azure.com"})
	primary.imageReads.Store(0)
	images.imageReads.Store(0)
	if _, err := store.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.DiscoverImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if primary.imageReads.Load() != 0 || images.imageReads.Load() != 0 ||
		images.override != "https://images.openai.azure.com" {
		t.Fatal("split discovery queried the Models API or lost its endpoint override")
	}
	if snapshot.ImageCatalogChecked || slices.Contains(snapshot.Names(foundry.Images), "synthetic") {
		t.Fatalf("secondary inventory contains Models API data: %+v", snapshot)
	}

	images.snapshot.Deployments[0].Source = foundry.ModelsAPISource
	if _, err := store.DiscoverImages(t.Context()); err == nil {
		t.Fatal("synthetic image entry was accepted as an ARM deployment")
	}
}

func TestSeparateImageFailuresAndResourceChangesAreIsolated(t *testing.T) {
	store, _, images := newSplitStore(t, Overrides{})
	store.SetImageDiscoveryError(errors.New("image permission denied"))
	if store.FoundryStatus().RefreshError != "" ||
		!strings.Contains(store.ImageFoundryStatus().RefreshError, "permission denied") {
		t.Fatal("image discovery error leaked into primary status")
	}
	if !store.HasChatCredentials() || !store.HasEmbeddingCredentials() || !store.HasImageCredentials() {
		t.Fatal("configured identities were not independently ready")
	}
	store.ConfigureImageFoundry(testImageResource, nil, errors.New("image identity unavailable"))
	if !store.HasChatCredentials() || !store.HasEmbeddingCredentials() || store.HasImageCredentials() {
		t.Fatal("image identity failure affected the wrong credential state")
	}
	if store.Get().ImageHost() != images.snapshot.Endpoint {
		t.Fatal("same-resource reconfiguration unexpectedly discarded its catalog")
	}

	other := strings.Replace(testImageResource, "/accounts/images", "/accounts/other-images", 1)
	store.ConfigureImageFoundry(other, images, nil)
	if store.Get().ImageHost() != "" || len(store.Get().ImageModels) != 0 {
		t.Fatal("a catalog was reused after the image resource changed")
	}
	if err := store.SetImageCatalog(images.snapshot); err == nil {
		t.Fatal("a catalog for the old image resource was accepted")
	}
	cfg := store.Get()
	cfg.ChatDeployment = "my-chat"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if !store.IsConfigured() {
		t.Fatal("an unavailable image resource disabled primary chat")
	}
}

func TestSeparateImageRoleValidationAndSameIDDeduplication(t *testing.T) {
	store, _, _ := newSplitStore(t, Overrides{})
	cfg := store.Get()
	cfg.ChatDeployment = "shared"
	cfg.EmbeddingDeployment = "my-vectors"
	cfg.ImageDeployment = "shared"
	if err := store.ValidateRoleSelections(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ImageDeployment = "my-chat"
	if err := store.ValidateRoleSelections(cfg); err == nil {
		t.Fatal("primary-only deployment was accepted for the image role")
	}
	cfg.ImageDeployment = "shared"
	store.mu.Lock()
	store.overrides.EmbeddingEndpoint = cfg.ImageHost()
	store.mu.Unlock()
	if err := store.ValidateRoleSelections(cfg); err == nil {
		t.Fatal("image endpoint was accepted for a primary embedding deployment")
	}
	store.mu.Lock()
	store.overrides.EmbeddingEndpoint = ""
	store.mu.Unlock()

	store.ConfigureImageFoundry(strings.ToUpper(testResource)+"/", nil, errors.New("ignored"))
	if store.HasSeparateImageResource() || store.ImageFoundryStatus().ResourceID != store.FoundryStatus().ResourceID {
		t.Fatal("same resource ID was not deduplicated")
	}
	if store.Get().ImageHost() != store.Get().Endpoint {
		t.Fatal("same-resource image configuration stopped using primary behavior")
	}
}

func TestSeparateImageEndpointOverrideMustMatchCatalog(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "config.json"), Keys{}, Overrides{
		ImageEndpoint: "https://expected.openai.azure.com",
	})
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	source := &imageTestSource{}
	store.ConfigureFoundry(testResource, source, nil)
	store.ConfigureImageFoundry(testImageResource, source, nil)
	snapshot := splitCatalog(testImageResource, "https://other.openai.azure.com/openai/v1")
	if err := store.SetImageCatalog(snapshot); err == nil {
		t.Fatal("catalog from an endpoint other than AZURE_IMAGE_ENDPOINT was accepted")
	}
	source.snapshot = snapshot
	if _, err := store.DiscoverImages(t.Context()); err == nil {
		t.Fatal("discovery returned a mismatched endpoint that could overwrite the persistent cache")
	}
	snapshot.Endpoint = "https://expected.openai.azure.com/openai/v1"
	if err := store.SetImageCatalog(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DiscoverImages(t.Context()); err == nil ||
		store.ImageFoundryStatus().Catalog.Endpoint != snapshot.Endpoint {
		t.Fatal("failed discovery replaced the last valid image catalog")
	}
}
