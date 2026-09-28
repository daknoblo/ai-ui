package config

import (
	"fmt"
	"slices"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

// Snapshot freezes manual settings and environment-only keys for one request.
func (s *Store) Snapshot() *Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bound := NewStore("", Keys{API: s.apiKey, Embedding: s.embeddingAPIKey, Image: s.imageAPIKey}, s.overrides)
	bound.cur = cloneConfig(s.cur)
	return bound
}

// ImageModels returns canonical models from activated, usable replicas only.
func (s *Store) ImageModels(op foundry.Operation) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var models []string
	for _, id := range s.availablePoolLocked(op) {
		target, err := s.targetLocked(op, id)
		if err == nil && !slices.Contains(models, target.Deployment.CanonicalModel()) {
			models = append(models, target.Deployment.CanonicalModel())
		}
	}
	slices.Sort(models)
	return models
}

func (s *Store) imageCandidatesLocked(op foundry.Operation, model string) []string {
	// Older saved chats contain a deployment alias. Migrate its meaning to
	// the canonical model, but never bypass activation of its replicas.
	if model != "" {
		if target, err := s.targetLocked(op, model); err == nil {
			model = target.Deployment.CanonicalModel()
		}
	}
	var ids []string
	for _, id := range s.availablePoolLocked(op) {
		target, err := s.targetLocked(op, id)
		if err == nil && (model == "" || target.Deployment.CanonicalModel() == model) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Store) ValidateImageSelection(op foundry.Operation, model string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.imageCandidatesLocked(op, model)) == 0 {
		return fmt.Errorf("image model %q has no enabled replica for %s", model, op)
	}
	return nil
}

// SelectImageRoute cycles either the entire pool or replicas of one model.
func (s *Store) SelectImageRoute(op foundry.Operation, model string) (*Store, foundry.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.imageCandidatesLocked(op, model)
	if len(ids) == 0 {
		return nil, foundry.Deployment{}, fmt.Errorf("image model %q has no enabled replica for %s", model, op)
	}
	if s.cycles == nil {
		s.cycles = make(map[foundry.Operation]uint64)
	}
	key := foundry.Operation(string(op) + ":" + model)
	id := ids[s.cycles[key]%uint64(len(ids))]
	s.cycles[key]++
	return s.selectRouteLocked(op, id)
}
