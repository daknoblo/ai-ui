package demo

import (
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/llm"
)

func TestSeparateImageResourceDemo(t *testing.T) {
	t.Setenv("AZURE_IMAGE_RESOURCE_ID", "ignored-external-account")
	t.Setenv("AZURE_IMAGE_ENDPOINT", "https://must-not-be-contacted.invalid/openai/v1")
	primary := testFoundryBackend(t, "en")
	images := testFoundryBackend(t, "en")
	cfg, store, _, err := SetupFoundry(t.Context(), t.TempDir(), "en", primary.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() }) // Preserve the test failure if closing fails.
	before, known, err := store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known {
		t.Fatalf("missing initial embedding profile: %v", err)
	}
	if err := ConfigureImageResource(t.Context(), cfg, store, images.URL()); err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSeparateImageResource() || cfg.Get().Endpoint != primary.URL()+"/openai/v1" ||
		cfg.Get().ImageHost() != images.URL()+"/openai/v1" {
		t.Fatal("demo did not isolate the two loopback endpoints")
	}
	deployment, err := cfg.ResolveDeployment(foundry.Images, "canvas")
	if err != nil || deployment.ModelName != "gpt-image-2" || deployment.Source == foundry.ModelsAPISource {
		t.Fatalf("image picker did not use the actual secondary deployment: %+v, %v", deployment, err)
	}
	snapshot, err := cfg.DiscoverImages(t.Context())
	if err != nil || snapshot.ResourceID != demoImageResourceID || snapshot.ImageCatalogChecked {
		t.Fatalf("image resource did not use its ARM-only catalog: %+v, %v", snapshot, err)
	}
	client := llm.New(cfg)
	if err := client.VerifyImage(t.Context(), "canvas"); err != nil {
		t.Fatal(err)
	}
	image, err := client.GenerateImage(t.Context(), "A blue park", llm.ImageOptions{})
	if err != nil || len(image.Data) == 0 {
		t.Fatalf("image generation failed: %v", err)
	}
	if _, err := client.EditImage(t.Context(), "Make it green",
		llm.ImageSource{Name: "demo.png", MIME: image.MIME, Data: image.Data}, llm.ImageOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyChat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedProfile(t.Context(), before, []string{"query"}); err != nil {
		t.Fatal(err)
	}
	after, _, err := store.ActiveEmbeddingProfile(t.Context())
	if err != nil || before != after {
		t.Fatal("image-only configuration changed the embedding profile")
	}
	cached, known, err := store.LoadCatalog(t.Context(), demoImageResourceID)
	if err != nil || !known || cached.Endpoint != snapshot.Endpoint {
		t.Fatal("the separate image catalog was not cached")
	}
}
