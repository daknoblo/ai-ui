package config

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

// FoundrySource is implemented by the Azure client and the credential-free demo.
type FoundrySource interface {
	Refresh(context.Context, string) (foundry.Snapshot, error)
	Authorize(*http.Request) error
}

type FoundryStatus struct {
	Enabled       bool
	ResourceID    string
	IdentityReady bool
	IdentityError string
	RefreshError  string
	Catalog       foundry.Snapshot
}

func (s *Store) ConfigureFoundry(resourceID string, source FoundrySource, setupErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceID = strings.TrimRight(strings.TrimSpace(resourceID), "/")
	s.identity = source
	s.identityError = ""
	if setupErr != nil {
		s.identityError = setupErr.Error()
	}
}

func (s *Store) FoundryStatus() FoundryStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return FoundryStatus{
		Enabled: s.resourceID != "", ResourceID: s.resourceID,
		IdentityReady: s.identity != nil && s.identityError == "",
		IdentityError: s.identityError, RefreshError: s.discoveryError,
		Catalog: cloneCatalog(s.catalog),
	}
}

func cloneCatalog(snapshot foundry.Snapshot) foundry.Snapshot {
	snapshot.Deployments = slices.Clone(snapshot.Deployments)
	for i := range snapshot.Deployments {
		snapshot.Deployments[i].Capabilities = maps.Clone(snapshot.Deployments[i].Capabilities)
	}
	return snapshot
}

func (s *Store) SetCatalog(snapshot foundry.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resourceID == "" || !strings.EqualFold(snapshot.ResourceID, s.resourceID) {
		return fmt.Errorf("deployment catalog belongs to a different resource")
	}
	if snapshot.Endpoint == "" || !foundry.IsV1Endpoint(snapshot.Endpoint) {
		return fmt.Errorf("deployment catalog has no supported inference endpoint")
	}
	s.catalog = cloneCatalog(snapshot)
	s.discoveryError = ""
	return nil
}

func (s *Store) Discover(ctx context.Context) (foundry.Snapshot, error) {
	s.mu.RLock()
	source, setupErr, override := s.identity, s.identityError, s.overrides.Endpoint
	s.mu.RUnlock()
	if setupErr != "" {
		return foundry.Snapshot{}, fmt.Errorf("identity configuration: %s", setupErr)
	}
	if source == nil {
		return foundry.Snapshot{}, fmt.Errorf("no Foundry identity configured")
	}
	return source.Refresh(ctx, override)
}

func (s *Store) SetDiscoveryError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoveryError = ""
	if err != nil {
		s.discoveryError = err.Error()
	}
}

func (s *Store) effectiveLocked() Config {
	cfg := s.overrides.apply(s.cur)
	if s.resourceID == "" {
		return cfg
	}
	cfg.Foundry = true
	cfg.ChatModel = s.cur.ChatModel
	cfg.Endpoint = s.catalog.Endpoint
	cfg.EmbeddingEndpoint = s.overrides.EmbeddingEndpoint
	cfg.ImageEndpoint = s.overrides.ImageEndpoint
	cfg.ChatModels = allowedModels(s.catalog.Names(foundry.Chat), s.overrides.ChatModels)
	cfg.ImageModels = allowedModels(s.catalog.Names(foundry.Images), s.overrides.ImageModels)
	cfg.ImageEditModels = allowedModels(s.catalog.Names(foundry.ImageEdits), s.overrides.ImageModels)
	return cfg
}

func allowedModels(discovered, pinned []string) []string {
	if len(pinned) == 0 {
		return discovered
	}
	out := make([]string, 0, len(pinned))
	for _, name := range pinned {
		if slices.Contains(discovered, name) {
			out = append(out, name)
		}
	}
	return out
}

func (s *Store) ResolveDeployment(op foundry.Operation, name string) (foundry.Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.resourceID == "" {
		return foundry.Deployment{Name: name, ModelName: name}, nil
	}
	deployment, ok := s.catalog.Find(name)
	if !ok {
		return foundry.Deployment{}, fmt.Errorf("deployment %q is not in the resource inventory", name)
	}
	if !deployment.Supports(op) {
		return foundry.Deployment{}, fmt.Errorf("deployment %q: %s", name, deployment.UnsupportedReason(op))
	}
	switch op {
	case foundry.Chat, foundry.Vision:
		if len(s.overrides.ChatModels) > 0 && !slices.Contains(s.overrides.ChatModels, name) {
			return foundry.Deployment{}, fmt.Errorf("deployment %q is excluded by AZURE_MODELS", name)
		}
	case foundry.Images, foundry.ImageEdits:
		if len(s.overrides.ImageModels) > 0 && !slices.Contains(s.overrides.ImageModels, name) {
			return foundry.Deployment{}, fmt.Errorf("deployment %q is excluded by AZURE_IMAGE_MODELS", name)
		}
	}
	return deployment, nil
}

func (s *Store) ModelIdentity(name string) string {
	status := s.FoundryStatus()
	if status.Enabled {
		if deployment, ok := status.Catalog.Find(name); ok {
			return deployment.ModelName
		}
	}
	return name
}

func (s *Store) HasChatCredentials() bool {
	status := s.FoundryStatus()
	if status.Enabled {
		return status.IdentityReady
	}
	return s.HasAPIKey()
}

func (s *Store) HasEmbeddingCredentials() bool {
	status := s.FoundryStatus()
	if status.Enabled {
		return status.IdentityReady
	}
	return s.HasEmbeddingAPIKey()
}

func (s *Store) HasImageCredentials() bool {
	status := s.FoundryStatus()
	if status.Enabled {
		return status.IdentityReady
	}
	return s.HasImageAPIKey()
}

func (s *Store) Authorize(req *http.Request, op foundry.Operation, deployment string) error {
	s.mu.RLock()
	enabled, source, setupErr, endpoint := s.resourceID != "", s.identity, s.identityError, s.catalog.Endpoint
	s.mu.RUnlock()
	if !enabled {
		key := s.APIKey()
		switch op {
		case foundry.Embeddings:
			key = s.EmbeddingAPIKey()
		case foundry.Images, foundry.ImageEdits:
			key = s.ImageAPIKey()
		}
		if key == "" {
			return fmt.Errorf("no API key configured")
		}
		req.Header.Set("api-key", key)
		return nil
	}
	if source == nil || setupErr != "" {
		return fmt.Errorf("foundry identity is not configured")
	}
	if _, err := s.ResolveDeployment(op, deployment); err != nil {
		return err
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || !strings.EqualFold(base.Host, req.URL.Host) ||
		base.Scheme != req.URL.Scheme || !strings.HasPrefix(req.URL.Path, strings.TrimRight(base.Path, "/")+"/") {
		return fmt.Errorf("inference endpoint does not match the discovered resource")
	}
	return source.Authorize(req)
}

func (s *Store) ValidateRoleSelections(cfg Config) error {
	if !s.FoundryStatus().Enabled {
		return nil
	}
	effective := s.Get()
	for _, endpoint := range []struct {
		name string
		host string
	}{
		{cfg.EmbeddingDeployment, effective.EmbeddingHost()},
		{cfg.ImageDeployment, effective.ImageHost()},
	} {
		if endpoint.name != "" && strings.TrimRight(endpoint.host, "/") != strings.TrimRight(effective.Endpoint, "/") {
			return fmt.Errorf("deployment %q has a dedicated endpoint that conflicts with the discovered Foundry resource", endpoint.name)
		}
	}
	for _, binding := range []struct {
		op   foundry.Operation
		name string
	}{
		{foundry.Chat, cfg.ChatDeployment},
		{foundry.Embeddings, cfg.EmbeddingDeployment},
		{foundry.Images, cfg.ImageDeployment},
		{foundry.Vision, cfg.VisionDeployment},
	} {
		if binding.name != "" {
			if _, err := s.ResolveDeployment(binding.op, binding.name); err != nil {
				return err
			}
		}
	}
	return nil
}
