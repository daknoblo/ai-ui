package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func (c *Client) VisionDeployment(chosen string) (string, bool) {
	cfg := c.store.Get()
	if !cfg.Foundry {
		return VisionModel(chosen, cfg.ChatModels)
	}
	if chosen == "" {
		chosen = cfg.ChatDeployment
	}
	if _, err := c.store.ResolveDeployment(foundry.Vision, chosen); err == nil {
		return chosen, true
	}
	if cfg.VisionDeployment != "" {
		if _, err := c.store.ResolveDeployment(foundry.Vision, cfg.VisionDeployment); err == nil {
			return cfg.VisionDeployment, true
		}
	}
	return "", false
}

func (c *Client) ConfiguredEmbeddingProfile() (storage.EmbeddingProfile, error) {
	cfg := c.store.Get()
	if cfg.EmbeddingDeployment == "" || cfg.EmbeddingHost() == "" {
		return storage.EmbeddingProfile{}, fmt.Errorf("no embedding deployment configured")
	}
	endpoint, err := config.NormalizeEmbeddingEndpoint(cfg.EmbeddingHost())
	if err != nil {
		return storage.EmbeddingProfile{}, err
	}
	profile := storage.EmbeddingProfile{
		Endpoint: endpoint, Deployment: strings.TrimSpace(cfg.EmbeddingDeployment),
	}
	if !IsV1Endpoint(endpoint) {
		profile.APIVersion = cfg.EmbeddingVersion()
	}
	if cfg.Foundry {
		deployment, err := c.store.ResolveDeployment(foundry.Embeddings, cfg.EmbeddingDeployment)
		if err != nil {
			return storage.EmbeddingProfile{}, err
		}
		profile.ResourceID = strings.ToLower(c.store.FoundryStatus().ResourceID)
		profile.ModelName = deployment.ModelName
		profile.ModelVersion = deployment.ModelVersion
	}
	return profile, nil
}

// EmbedProfile uses the index's model, not a concurrently changed UI selection.
func (c *Client) EmbedProfile(ctx context.Context, profile storage.EmbeddingProfile, inputs []string) ([][]float32, error) {
	endpoint, err := config.NormalizeEmbeddingEndpoint(profile.Endpoint)
	if err != nil {
		return nil, err
	}
	if !IsV1Endpoint(endpoint) && strings.TrimSpace(profile.APIVersion) == "" {
		return nil, fmt.Errorf("embedding profile has no pinned API version; configure the API version and rebuild the document index")
	}
	cfg := c.store.Get()
	if cfg.Foundry != (profile.ResourceID != "") {
		return nil, fmt.Errorf("embedding authentication mode changed; rebuild the document index")
	}
	if profile.ResourceID != "" {
		if !strings.EqualFold(profile.ResourceID, c.store.FoundryStatus().ResourceID) {
			return nil, fmt.Errorf("embedding index belongs to a different Azure resource")
		} else {
			deployment, err := c.store.ResolveDeployment(foundry.Embeddings, profile.Deployment)
			if err != nil {
				return nil, err
			}
			if deployment.ModelName != profile.ModelName || deployment.ModelVersion != profile.ModelVersion {
				return nil, fmt.Errorf("embedding deployment changed; rebuild the document index")
			}
		}
	}
	cfg.EmbeddingEndpoint = endpoint
	cfg.EmbeddingDeployment = profile.Deployment
	cfg.EmbeddingAPIVersion = profile.APIVersion
	cfg.APIVersion = profile.APIVersion
	vectors, err := c.embed(ctx, cfg, inputs)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(inputs) {
		return nil, fmt.Errorf("embedding response count does not match the input")
	}
	for _, vector := range vectors {
		if len(vector) == 0 || (profile.Dimensions > 0 && len(vector) != profile.Dimensions) {
			return nil, fmt.Errorf("embedding dimensions changed; rebuild the document index")
		}
	}
	return vectors, nil
}
