package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrReindexRequired   = errors.New("the embedding index must be rebuilt before changing its profile")
	ErrReindexInProgress = errors.New("an embedding reindex is in progress")
	ErrReindexNotRunning = errors.New("the embedding reindex job is not running")
)

// EmbeddingProfile identifies the model that produced an index's vectors.
// Dimensions is zero until an embedding response establishes the vector size.
type EmbeddingProfile struct {
	ResourceID   string `json:"resource_id"`
	Endpoint     string `json:"endpoint"`
	Deployment   string `json:"deployment"`
	ModelName    string `json:"model_name"`
	ModelVersion string `json:"model_version"`
	Dimensions   int    `json:"dimensions"`
}

// SameIdentity deliberately does not compare dimensions: different models can
// produce equally sized vectors that cannot be compared to one another.
func (p EmbeddingProfile) SameIdentity(other EmbeddingProfile) bool {
	return strings.EqualFold(strings.TrimRight(p.ResourceID, "/"), strings.TrimRight(other.ResourceID, "/")) &&
		strings.TrimRight(p.Endpoint, "/") == strings.TrimRight(other.Endpoint, "/") &&
		p.Deployment == other.Deployment &&
		p.ModelName == other.ModelName &&
		p.ModelVersion == other.ModelVersion
}

// ActiveEmbeddingProfile reads the persisted profile without inferring the
// identity of legacy vectors. Use WithEmbeddingProfile when also using vectors.
func (s *Store) ActiveEmbeddingProfile(ctx context.Context) (EmbeddingProfile, bool, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT profile FROM embedding_profile WHERE id = 1`).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbeddingProfile{}, false, nil
	}
	if err != nil {
		return EmbeddingProfile{}, false, err
	}
	var profile EmbeddingProfile
	if err := json.Unmarshal([]byte(encoded), &profile); err != nil {
		return EmbeddingProfile{}, false, fmt.Errorf("decode active embedding profile: %w", err)
	}
	return profile, true, nil
}

// WithEmbeddingProfile holds the active generation stable throughout fn,
// including any inference call. Acquire corpus mutation access first if needed;
// fn must not acquire it or attempt to switch the active profile.
func (s *Store) WithEmbeddingProfile(ctx context.Context, fn func(EmbeddingProfile, bool) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.embeddingMu.RLock()
	defer s.embeddingMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	profile, known, err := s.ActiveEmbeddingProfile(ctx)
	if err != nil {
		return err
	}
	return fn(profile, known)
}

// WithCorpusMutation prevents a reindex snapshot from overlapping an upload or
// deletion. The entire mutation, including inference, must happen inside fn.
// Callbacks must not nest corpus mutation access or start/finish a reindex.
func (s *Store) WithCorpusMutation(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.corpusMu.RLock()
	defer s.corpusMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	running, err := s.reindexRunning(ctx)
	if err != nil {
		return err
	}
	if running {
		return ErrReindexInProgress
	}
	return fn()
}

// SetInitialEmbeddingProfile only labels an empty index. Existing vectors,
// including legacy vectors with no known profile, must go through reindexing.
func (s *Store) SetInitialEmbeddingProfile(ctx context.Context, profile EmbeddingProfile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if profile.Dimensions < 0 {
		return fmt.Errorf("embedding dimensions must not be negative")
	}
	// Exclusive corpus access also prevents a concurrent first upload from
	// inserting vectors between the emptiness check and the profile change.
	s.corpusMu.Lock()
	defer s.corpusMu.Unlock()
	s.embeddingMu.Lock()
	defer s.embeddingMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // No-op after a successful commit.

	var running bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM embedding_reindex_jobs WHERE status = 'running')`).Scan(&running); err != nil {
		return err
	}
	if running {
		return ErrReindexInProgress
	}
	var chunks int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&chunks); err != nil {
		return err
	}
	if chunks != 0 {
		return ErrReindexRequired
	}
	if err := writeEmbeddingProfile(ctx, tx, profile); err != nil {
		return err
	}
	return tx.Commit()
}

// EmbeddingCounts reads both corpus counts from the same SQLite snapshot.
func (s *Store) EmbeddingCounts(ctx context.Context) (documents int, chunks int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM chunks)`).Scan(&documents, &chunks)
	return documents, chunks, err
}

func (s *Store) reindexRunning(ctx context.Context) (bool, error) {
	var running bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM embedding_reindex_jobs WHERE status = 'running')`).Scan(&running)
	return running, err
}

func writeEmbeddingProfile(ctx context.Context, tx *sql.Tx, profile EmbeddingProfile) error {
	encoded, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("encode embedding profile: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO embedding_profile (id, profile) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET profile = excluded.profile`, string(encoded))
	return err
}
