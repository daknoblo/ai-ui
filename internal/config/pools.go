package config

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

var ErrEmbeddingPool = errors.New("embedding replicas must have identical known model, version and dimensions")

var Operations = []foundry.Operation{foundry.Chat, foundry.Vision, foundry.Images, foundry.ImageEdits, foundry.Embeddings}

func (s *Store) ModelFilters() (chat, images bool) {
	return len(s.overrides.ChatModels) > 0, len(s.overrides.ImageModels) > 0
}

func (s *Store) poolReady(op foundry.Operation) (pooled, ready bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pooled = s.resourceID != "" && s.cur.EnabledDeployments != nil
	return pooled, pooled && len(s.availablePoolLocked(op)) > 0
}

type Target struct {
	Key        string
	Account    foundry.Snapshot
	Deployment foundry.Deployment
	source     FoundrySource
}

func cloneConfig(cfg Config) Config {
	if cfg.EnabledDeployments != nil {
		pools := make(map[foundry.Operation][]string, len(cfg.EnabledDeployments))
		for op, ids := range cfg.EnabledDeployments {
			pools[op] = slices.Clone(ids)
		}
		cfg.EnabledDeployments = pools
	}
	return cfg
}

func first(ids []string) string {
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func refreshSource(ctx context.Context, source FoundrySource, override string, previous foundry.Snapshot) (foundry.Snapshot, error) {
	if group, ok := source.(interface {
		RefreshGroup(context.Context, string, foundry.Snapshot) (foundry.Snapshot, error)
	}); ok {
		return group.RefreshGroup(ctx, override, previous)
	}
	return source.Refresh(ctx, override)
}

func validateAccounts(root foundry.Snapshot) error {
	seen := map[string]bool{strings.ToLower(root.ResourceID): true}
	for _, account := range root.Accounts {
		id := strings.ToLower(account.ResourceID)
		if !foundry.SameGroup(root.ResourceID, account.ResourceID) || seen[id] ||
			len(account.Accounts) != 0 || !foundry.IsV1Endpoint(account.Endpoint) {
			return fmt.Errorf("discovered account is duplicate, out of scope or has no supported endpoint")
		}
		seen[id] = true
	}
	return nil
}

func (s *Store) targetsLocked() []Target {
	var targets []Target
	seen := map[string]bool{}
	primarySource, imageSource := s.identity, s.imageIdentity
	if s.identityError != "" {
		primarySource = nil
	}
	if s.imageIdentityError != "" {
		imageSource = nil
	}
	appendAccount := func(account foundry.Snapshot, source FoundrySource) {
		account.Accounts = nil
		for _, d := range account.Deployments {
			key := account.Key(d)
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, Target{Key: key, Account: account, Deployment: d, source: source})
		}
	}
	appendAccount(s.catalog, primarySource)
	appendAccount(s.imageCatalog, imageSource)
	for _, account := range s.catalog.Accounts {
		appendAccount(account, primarySource)
	}
	for _, account := range s.imageCatalog.Accounts {
		appendAccount(account, imageSource)
	}
	return targets
}

func (s *Store) targetLocked(op foundry.Operation, id string) (Target, error) {
	if !strings.HasPrefix(id, "/") {
		resource := s.resourceID
		if (op == foundry.Images || op == foundry.ImageEdits) && s.separateImageResourceLocked() {
			resource = s.imageResourceID
		}
		id = strings.ToLower(resource) + "/deployments/" + id
	}
	for _, target := range s.targetsLocked() {
		if target.Key != id {
			continue
		}
		if !target.Deployment.Supports(op) {
			return Target{}, fmt.Errorf("deployment %q does not support %s", id, op)
		}
		filter, endpoint := s.overrides.ChatModels, s.overrides.Endpoint
		switch op {
		case foundry.Embeddings:
			filter, endpoint = nil, s.overrides.EmbeddingEndpoint
		case foundry.Images, foundry.ImageEdits:
			filter, endpoint = s.overrides.ImageModels, s.overrides.ImageEndpoint
		}
		if len(filter) > 0 && !slices.Contains(filter, target.Deployment.Name) && !slices.Contains(filter, id) {
			return Target{}, fmt.Errorf("deployment %q is excluded by the environment model filter", id)
		}
		if endpoint != "" && strings.TrimRight(endpoint, "/") != target.Account.Endpoint {
			normalized, err := foundry.NormalizeEndpoint(endpoint)
			if err != nil || normalized != target.Account.Endpoint {
				return Target{}, fmt.Errorf("deployment %q conflicts with the environment endpoint", id)
			}
		}
		return target, nil
	}
	return Target{}, fmt.Errorf("deployment %q is not in the resource inventory", id)
}

func (s *Store) targetKeysLocked(op foundry.Operation) []string {
	var keys []string
	for _, target := range s.targetsLocked() {
		if _, err := s.targetLocked(op, target.Key); err == nil {
			keys = append(keys, target.Key)
		}
	}
	slices.Sort(keys)
	return keys
}

func (s *Store) Targets(op foundry.Operation) []Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var targets []Target
	for _, key := range s.targetKeysLocked(op) {
		target, err := s.targetLocked(op, key)
		if err == nil {
			target.Account = cloneCatalog(target.Account)
			target.Deployment.Capabilities = cloneCatalog(foundry.Snapshot{Deployments: []foundry.Deployment{target.Deployment}}).Deployments[0].Capabilities
			targets = append(targets, target)
		}
	}
	return targets
}

func (s *Store) legacyLocked(op foundry.Operation, cfg Config) string {
	cfg = s.overrides.apply(cfg)
	switch op {
	case foundry.Chat:
		return cfg.ChatDeployment
	case foundry.Vision:
		if cfg.VisionDeployment != "" {
			return cfg.VisionDeployment
		}
		return cfg.ChatDeployment
	case foundry.Embeddings:
		return cfg.EmbeddingDeployment
	default:
		return cfg.ImageDeployment
	}
}

func (s *Store) poolLocked(op foundry.Operation) []string {
	ids := s.cur.EnabledDeployments[op]
	locked := s.poolLockedByEnv(op)
	if s.cur.EnabledDeployments == nil || locked {
		name := s.legacyLocked(op, s.cur)
		if name == "" {
			return nil
		}
		if target, err := s.targetLocked(op, name); err == nil {
			return []string{target.Key}
		}
		return []string{name}
	}
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if target, err := s.targetLocked(op, id); err == nil {
			id = target.Key
		}
		if !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	return unique
}

func (s *Store) poolLockedByEnv(op foundry.Operation) bool {
	switch op {
	case foundry.Chat:
		return s.locks.ChatDeployment
	case foundry.Embeddings:
		return s.locks.EmbeddingDeployment
	case foundry.Images, foundry.ImageEdits:
		return s.locks.ImageDeployment
	}
	return false
}

func (s *Store) EnabledPools() map[foundry.Operation][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pools := make(map[foundry.Operation][]string)
	for _, op := range Operations {
		pools[op] = s.poolLocked(op)
	}
	return pools
}

func (s *Store) availablePoolLocked(op foundry.Operation) []string {
	var ids []string
	for _, id := range s.poolLocked(op) {
		if target, err := s.targetLocked(op, id); err == nil && target.source != nil {
			ids = append(ids, target.Key)
		}
	}
	return ids
}

func (s *Store) validatePoolsLocked(cfg Config) error {
	if cfg.EnabledDeployments == nil || s.resourceID == "" {
		return nil
	}
	for op, ids := range cfg.EnabledDeployments {
		if !slices.Contains(Operations, op) {
			return fmt.Errorf("unknown deployment operation %q", op)
		}
		var reference foundry.Deployment
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			target, err := s.targetLocked(op, id)
			if err != nil {
				return err
			}
			if op == foundry.Embeddings && reference.Name != "" {
				model, dimensions := target.Deployment.EmbeddingSpace()
				previousModel, previousDimensions := reference.EmbeddingSpace()
				if model == "" || dimensions == 0 || model != previousModel ||
					dimensions != previousDimensions || target.Deployment.ModelVersion != reference.ModelVersion {
					return ErrEmbeddingPool
				}
			}
			reference = target.Deployment
		}
	}
	return nil
}

// SelectRoute atomically chooses one provider and freezes configuration,
// catalog and authorization together. Call once per user turn/provider operation.
// Explicit legacy names are always anchored to their original configured account.
func (s *Store) SelectRoute(op foundry.Operation, pin string) (*Store, foundry.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectRouteLocked(op, pin)
}

func (s *Store) selectRouteLocked(op foundry.Operation, pin string) (*Store, foundry.Deployment, error) {
	cfg := s.effectiveLocked()
	imageIdentity := (op == foundry.Images || op == foundry.ImageEdits) && cfg.SeparateImageResource
	if !cfg.Foundry && !imageIdentity {
		return s, foundry.Deployment{Name: pin, ModelName: pin}, nil
	}
	if cfg.EnabledDeployments != nil && len(s.poolLocked(op)) == 0 {
		return nil, foundry.Deployment{}, fmt.Errorf("no enabled deployment for %s", op)
	}
	if pin == "" {
		ids := s.availablePoolLocked(op)
		if len(ids) == 0 {
			return nil, foundry.Deployment{}, fmt.Errorf("no enabled deployment for %s", op)
		}
		if s.cycles == nil {
			s.cycles = make(map[foundry.Operation]uint64)
		}
		pin = ids[s.cycles[op]%uint64(len(ids))]
		s.cycles[op]++
	}
	target, err := s.targetLocked(op, pin)
	if err != nil {
		return nil, foundry.Deployment{}, err
	}
	if target.source == nil {
		return nil, foundry.Deployment{}, fmt.Errorf("foundry identity is not configured")
	}
	// This isolated store is never saved. It reuses the immutable credential,
	// not the mutable catalog of the live settings store.
	bound := NewStore("", Keys{API: s.apiKey, Embedding: s.embeddingAPIKey, Image: s.imageAPIKey}, Overrides{})
	bound.cur = cfg
	bound.cur.EnabledDeployments = nil
	bound.cur.Endpoint = target.Account.Endpoint
	bound.cur.EmbeddingEndpoint = target.Account.Endpoint
	bound.cur.ImageEndpoint = target.Account.Endpoint
	bound.cur.ChatDeployment = target.Deployment.Name
	bound.cur.ChatModel = target.Deployment.Name
	bound.cur.EmbeddingDeployment = target.Deployment.Name
	bound.cur.ImageDeployment = target.Deployment.Name
	bound.resourceID = target.Account.ResourceID
	bound.catalog = cloneCatalog(target.Account)
	bound.identity = target.source
	return bound, target.Deployment, nil
}

// SelectEmbeddingRoute only cycles among replicas compatible with the index.
// An older index may keep using its explicitly inventoried source until the
// staged reindex commits. Empty pools remain disabled.
func (s *Store) SelectEmbeddingRoute(resource, deployment, model, version string, dimensions int) (*Store, foundry.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.poolLocked(foundry.Embeddings)
	if len(ids) == 0 {
		return nil, foundry.Deployment{}, fmt.Errorf("no enabled embedding deployment")
	}
	compatible := make([]string, 0, len(ids))
	for _, id := range ids {
		target, err := s.targetLocked(foundry.Embeddings, id)
		if err != nil {
			continue
		}
		targetModel, targetDimensions := target.Deployment.EmbeddingSpace()
		if targetModel != "" && target.Deployment.EmbeddingModelMatches(model, version) &&
			dimensions == targetDimensions {
			compatible = append(compatible, id)
		}
	}
	if len(compatible) > 0 {
		if s.cycles == nil {
			s.cycles = map[foundry.Operation]uint64{}
		}
		id := compatible[s.cycles[foundry.Embeddings]%uint64(len(compatible))]
		s.cycles[foundry.Embeddings]++
		return s.selectRouteLocked(foundry.Embeddings, id)
	}
	return s.selectRouteLocked(foundry.Embeddings,
		strings.ToLower(strings.TrimRight(resource, "/"))+"/deployments/"+deployment)
}
