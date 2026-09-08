package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
)

const (
	mainTestResource      = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/primary"
	mainTestImageResource = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/images"
)

func mainTestStore(t *testing.T) *config.Store {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{}, config.Overrides{})
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestConfigureFoundryFromEnvClonesImageResource(t *testing.T) {
	store := mainTestStore(t)
	env := map[string]string{
		"AZURE_RESOURCE_ID":       mainTestResource,
		"AZURE_IMAGE_RESOURCE_ID": mainTestImageResource,
		"AZURE_TENANT_ID":         "00000000-0000-0000-0000-000000000002",
		"AZURE_CLIENT_ID":         "00000000-0000-0000-0000-000000000003",
		"AZURE_CLIENT_SECRET":     "not-a-real-secret",
	}
	configureFoundryFromEnv(store, func(key string) string { return env[key] })
	if !store.FoundryStatus().IdentityReady || !store.ImageFoundryStatus().IdentityReady ||
		!store.HasSeparateImageResource() ||
		store.ImageFoundryStatus().ResourceID != mainTestImageResource {
		t.Fatalf("environment resources were not configured independently: primary=%+v image=%+v",
			store.FoundryStatus(), store.ImageFoundryStatus())
	}
}

func TestConfigureFoundryFromEnvRequiresPrimaryResource(t *testing.T) {
	store := mainTestStore(t)
	configureFoundryFromEnv(store, func(key string) string {
		if key == "AZURE_IMAGE_RESOURCE_ID" {
			return mainTestImageResource
		}
		return ""
	})
	status := store.ImageFoundryStatus()
	if !store.HasSeparateImageResource() || status.IdentityReady ||
		!strings.Contains(status.IdentityError, "requires AZURE_RESOURCE_ID") {
		t.Fatalf("missing primary resource was not surfaced: %+v", status)
	}
}

func TestConfigureFoundryFromEnvInvalidImageDoesNotDestroyPrimary(t *testing.T) {
	store := mainTestStore(t)
	env := map[string]string{
		"AZURE_RESOURCE_ID":       mainTestResource,
		"AZURE_IMAGE_RESOURCE_ID": "not-a-resource-id",
		"AZURE_TENANT_ID":         "00000000-0000-0000-0000-000000000002",
		"AZURE_CLIENT_ID":         "00000000-0000-0000-0000-000000000003",
		"AZURE_CLIENT_SECRET":     "not-a-real-secret",
	}
	configureFoundryFromEnv(store, func(key string) string { return env[key] })
	if !store.FoundryStatus().IdentityReady || store.ImageFoundryStatus().IdentityReady ||
		store.ImageFoundryStatus().IdentityError == "" {
		t.Fatalf("invalid image resource affected primary identity: primary=%+v image=%+v",
			store.FoundryStatus(), store.ImageFoundryStatus())
	}
}
