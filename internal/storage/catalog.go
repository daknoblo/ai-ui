package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

// SaveCatalog replaces only a complete, successful discovery snapshot.
func (s *Store) SaveCatalog(ctx context.Context, snapshot foundry.Snapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode deployment catalog: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO model_catalog (resource_id, snapshot)
		VALUES (?, ?) ON CONFLICT(resource_id) DO UPDATE SET snapshot = excluded.snapshot`,
		snapshot.ResourceID, string(data))
	return err
}

func (s *Store) LoadCatalog(ctx context.Context, resourceID string) (foundry.Snapshot, bool, error) {
	var data string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot FROM model_catalog WHERE resource_id = ?`, resourceID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return foundry.Snapshot{}, false, nil
	}
	if err != nil {
		return foundry.Snapshot{}, false, err
	}
	var snapshot foundry.Snapshot
	if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
		return foundry.Snapshot{}, false, fmt.Errorf("decode deployment catalog: %w", err)
	}
	if snapshot.ResourceID != resourceID {
		return foundry.Snapshot{}, false, fmt.Errorf("deployment catalog resource does not match")
	}
	return snapshot, true, nil
}
