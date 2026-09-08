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
	ImageModels(context.Context, string) ([]foundry.Deployment, error)
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
	s.embeddingEndpoints = nil
	resourceID = normalizeResourceID(resourceID)
	if !strings.EqualFold(resourceID, s.resourceID) {
		s.catalog = foundry.Snapshot{}
		s.discoveryError = ""
	}
	s.resourceID = resourceID
	s.identity = source
	s.identityError = ""
	if setupErr != nil {
		s.identityError = setupErr.Error()
	}
}

// ConfigureImageFoundry configures an optional identity-backed account used
// only for image operations.
func (s *Store) ConfigureImageFoundry(resourceID string, source FoundrySource, setupErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resourceID = normalizeResourceID(resourceID)
	if resourceID != "" {
		if err := foundry.ValidateResourceID(resourceID); err != nil && setupErr == nil {
			setupErr = err
			source = nil
		}
	}
	if !strings.EqualFold(resourceID, s.imageResourceID) {
		s.imageCatalog = foundry.Snapshot{}
		s.imageDiscoveryError = ""
	}
	s.imageResourceID = resourceID
	s.imageIdentity = source
	s.imageIdentityError = ""
	if setupErr != nil {
		s.imageIdentityError = setupErr.Error()
	}
}

func normalizeResourceID(resourceID string) string {
	return strings.TrimRight(strings.TrimSpace(resourceID), "/")
}

func (s *Store) separateImageResourceLocked() bool {
	return s.imageResourceID != "" && !strings.EqualFold(s.imageResourceID, s.resourceID)
}

// HasSeparateImageResource reports whether image operations use an account
// distinct from the primary Foundry account.
func (s *Store) HasSeparateImageResource() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.separateImageResourceLocked()
}

func (s *Store) FoundryStatus() FoundryStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.foundryStatusLocked()
}

func (s *Store) foundryStatusLocked() FoundryStatus {
	return FoundryStatus{
		Enabled: s.resourceID != "", ResourceID: s.resourceID,
		IdentityReady: s.identity != nil && s.identityError == "",
		IdentityError: s.identityError, RefreshError: s.discoveryError,
		Catalog: cloneCatalog(s.catalog),
	}
}

// ImageFoundryStatus returns the independent image account status when one is
// configured, and otherwise aliases the primary account status.
func (s *Store) ImageFoundryStatus() FoundryStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.separateImageResourceLocked() {
		return s.foundryStatusLocked()
	}
	return FoundryStatus{
		Enabled: s.imageResourceID != "", ResourceID: s.imageResourceID,
		IdentityReady: s.imageIdentity != nil && s.imageIdentityError == "",
		IdentityError: s.imageIdentityError, RefreshError: s.imageDiscoveryError,
		Catalog: cloneCatalog(s.imageCatalog),
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

// SetImageCatalog installs an ARM deployment snapshot for the separate image
// account. Synthetic Models API entries are never valid in this inventory.
func (s *Store) SetImageCatalog(snapshot foundry.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.separateImageResourceLocked() {
		return fmt.Errorf("no separate image resource is configured")
	}
	if err := foundry.ValidateResourceID(s.imageResourceID); err != nil {
		return fmt.Errorf("configured image resource ID is invalid")
	}
	if !strings.EqualFold(snapshot.ResourceID, s.imageResourceID) {
		return fmt.Errorf("image deployment catalog belongs to a different resource")
	}
	if snapshot.Endpoint == "" || !foundry.IsV1Endpoint(snapshot.Endpoint) {
		return fmt.Errorf("image deployment catalog has no supported inference endpoint")
	}
	if err := validateImageCatalog(snapshot); err != nil {
		return err
	}
	if err := validateImageEndpointOverride(snapshot.Endpoint, s.overrides.ImageEndpoint); err != nil {
		return err
	}
	s.imageCatalog = cloneCatalog(snapshot)
	s.imageDiscoveryError = ""
	return nil
}

func validateImageCatalog(snapshot foundry.Snapshot) error {
	for _, deployment := range snapshot.Deployments {
		if deployment.Source != "" {
			return fmt.Errorf("image deployment catalog contains a non-ARM model reference")
		}
	}
	return nil
}

func validateImageEndpointOverride(endpoint, override string) error {
	if override == "" {
		return nil
	}
	normalized, err := foundry.NormalizeEndpoint(override)
	if err != nil || normalized != endpoint {
		return fmt.Errorf("image deployment catalog endpoint does not match AZURE_IMAGE_ENDPOINT")
	}
	return nil
}

func (s *Store) Discover(ctx context.Context) (foundry.Snapshot, error) {
	s.mu.RLock()
	source, setupErr, override := s.identity, s.identityError, s.overrides.Endpoint
	separateImages := s.separateImageResourceLocked()
	s.mu.RUnlock()
	if setupErr != "" {
		return foundry.Snapshot{}, fmt.Errorf("identity configuration: %s", setupErr)
	}
	if source == nil {
		return foundry.Snapshot{}, fmt.Errorf("no Foundry identity configured")
	}
	snapshot, err := source.Refresh(ctx, override)
	if err != nil {
		return foundry.Snapshot{}, err
	}
	if separateImages {
		return snapshot, nil
	}
	images, imageErr := source.ImageModels(ctx, snapshot.Endpoint)
	if err := ctx.Err(); err != nil {
		return foundry.Snapshot{}, err
	}
	snapshot.ImageCatalogChecked = true
	if imageErr != nil {
		images = nil
		snapshot.ImageCatalogError = imageErr.Error()
		previous := s.FoundryStatus().Catalog
		if strings.EqualFold(previous.ResourceID, snapshot.ResourceID) && previous.Endpoint == snapshot.Endpoint {
			for _, model := range previous.Deployments {
				if model.Source == foundry.ModelsAPISource {
					images = append(images, model)
				}
			}
		}
	}
	for _, image := range images {
		if image.Source != foundry.ModelsAPISource || !image.Supports(foundry.Images) {
			return foundry.Snapshot{}, fmt.Errorf("image catalog returned an invalid model reference")
		}
		exists := slices.ContainsFunc(snapshot.Deployments, func(deployment foundry.Deployment) bool {
			return strings.EqualFold(deployment.Name, image.Name)
		})
		if !exists {
			snapshot.Deployments = append(snapshot.Deployments, image)
		}
	}
	return snapshot, nil
}

// DiscoverImages refreshes only the ARM account and deployment inventory for
// the separate image resource. It deliberately never queries the Models API.
func (s *Store) DiscoverImages(ctx context.Context) (foundry.Snapshot, error) {
	s.mu.RLock()
	separate := s.separateImageResourceLocked()
	resourceID, source := s.imageResourceID, s.imageIdentity
	setupErr, override := s.imageIdentityError, s.overrides.ImageEndpoint
	s.mu.RUnlock()
	if !separate {
		return foundry.Snapshot{}, fmt.Errorf("no separate image resource is configured")
	}
	if setupErr != "" {
		return foundry.Snapshot{}, fmt.Errorf("image identity configuration: %s", setupErr)
	}
	if source == nil {
		return foundry.Snapshot{}, fmt.Errorf("no image Foundry identity configured")
	}
	snapshot, err := source.Refresh(ctx, override)
	if err != nil {
		return foundry.Snapshot{}, err
	}
	if !strings.EqualFold(snapshot.ResourceID, resourceID) {
		return foundry.Snapshot{}, fmt.Errorf("image discovery returned a different resource")
	}
	if snapshot.Endpoint == "" || !foundry.IsV1Endpoint(snapshot.Endpoint) {
		return foundry.Snapshot{}, fmt.Errorf("image discovery returned no supported inference endpoint")
	}
	if err := validateImageCatalog(snapshot); err != nil {
		return foundry.Snapshot{}, err
	}
	if err := validateImageEndpointOverride(snapshot.Endpoint, override); err != nil {
		return foundry.Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) SetDiscoveryError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoveryError = ""
	if err != nil {
		s.discoveryError = err.Error()
	}
}

// SetImageDiscoveryError records a refresh failure independently of the
// primary resource.
func (s *Store) SetImageDiscoveryError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imageDiscoveryError = ""
	if err != nil {
		s.imageDiscoveryError = err.Error()
	}
}

func (s *Store) effectiveLocked() Config {
	cfg := s.overrides.apply(s.cur)
	cfg.Foundry = s.resourceID != ""
	cfg.SeparateImageResource = s.separateImageResourceLocked()
	if s.resourceID == "" {
		if cfg.SeparateImageResource {
			cfg.ImageEndpoint = s.imageCatalog.Endpoint
			cfg.ImageModels = allowedModels(s.imageCatalog.Names(foundry.Images), s.overrides.ImageModels)
			cfg.ImageEditModels = allowedModels(s.imageCatalog.Names(foundry.ImageEdits), s.overrides.ImageModels)
		}
		return cfg
	}
	cfg.Foundry = true
	cfg.ChatModel = s.cur.ChatModel
	cfg.Endpoint = s.catalog.Endpoint
	cfg.EmbeddingEndpoint = s.overrides.EmbeddingEndpoint
	cfg.ChatModels = allowedModels(s.catalog.Names(foundry.Chat), s.overrides.ChatModels)
	imageCatalog := s.catalog
	cfg.ImageEndpoint = s.overrides.ImageEndpoint
	if cfg.SeparateImageResource {
		imageCatalog = s.imageCatalog
		cfg.ImageEndpoint = s.imageCatalog.Endpoint
	}
	cfg.ImageModels = allowedModels(imageCatalog.Names(foundry.Images), s.overrides.ImageModels)
	cfg.ImageEditModels = allowedModels(imageCatalog.Names(foundry.ImageEdits), s.overrides.ImageModels)
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
	imageOperation := op == foundry.Images || op == foundry.ImageEdits
	separateImages := imageOperation && s.separateImageResourceLocked()
	if s.resourceID == "" && !separateImages {
		return foundry.Deployment{Name: name, ModelName: name}, nil
	}
	catalog := s.catalog
	if separateImages {
		catalog = s.imageCatalog
	}
	deployment, ok := catalog.Find(name)
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
	status := s.ImageFoundryStatus()
	if status.Enabled {
		return status.IdentityReady
	}
	return s.HasImageAPIKey()
}

func (s *Store) Authorize(req *http.Request, op foundry.Operation, deployment string) error {
	s.mu.RLock()
	enabled, source, setupErr, endpoint := s.resourceID != "", s.identity, s.identityError, s.catalog.Endpoint
	if (op == foundry.Images || op == foundry.ImageEdits) && s.separateImageResourceLocked() {
		enabled, source, setupErr, endpoint = true, s.imageIdentity, s.imageIdentityError, s.imageCatalog.Endpoint
	}
	s.mu.RUnlock()
	if !enabled {
		if op == foundry.Embeddings {
			return s.authorizeEmbeddingKey(req, deployment)
		}
		key := s.APIKey()
		switch op {
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
	primary, images := s.FoundryStatus(), s.ImageFoundryStatus()
	if !primary.Enabled && !s.HasSeparateImageResource() {
		return nil
	}
	effective := s.Get()
	primaryCatalogReady := primary.Catalog.Endpoint != "" &&
		strings.EqualFold(primary.Catalog.ResourceID, primary.ResourceID)
	if primaryCatalogReady && cfg.EmbeddingDeployment != "" &&
		strings.TrimRight(effective.EmbeddingHost(), "/") != strings.TrimRight(primary.Catalog.Endpoint, "/") {
		return fmt.Errorf("deployment %q has a dedicated endpoint that conflicts with the discovered Foundry resource", cfg.EmbeddingDeployment)
	}
	imageCatalogReady := images.Catalog.Endpoint != "" &&
		strings.EqualFold(images.Catalog.ResourceID, images.ResourceID)
	if imageCatalogReady && cfg.ImageDeployment != "" &&
		strings.TrimRight(effective.ImageHost(), "/") != strings.TrimRight(images.Catalog.Endpoint, "/") {
		return fmt.Errorf("deployment %q has a dedicated endpoint that conflicts with the discovered image Foundry resource", cfg.ImageDeployment)
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
		imageOperation := binding.op == foundry.Images || binding.op == foundry.ImageEdits
		if binding.name != "" && ((imageOperation && imageCatalogReady) || (!imageOperation && primaryCatalogReady)) {
			if _, err := s.ResolveDeployment(binding.op, binding.name); err != nil {
				return err
			}
		}
	}
	return nil
}
