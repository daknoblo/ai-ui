package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	ReindexRunning   = "running"
	ReindexSucceeded = "succeeded"
	ReindexFailed    = "failed"

	maxReindexBatchSize = 16
)

// ReindexJob persists the target identity, snapshot size and staging progress.
// FinishedAt remains zero while the job is running.
type ReindexJob struct {
	ID              int64            `json:"id"`
	Profile         EmbeddingProfile `json:"profile"`
	Status          string           `json:"status"`
	TotalChunks     int              `json:"total_chunks"`
	CompletedChunks int              `json:"completed_chunks"`
	Documents       int              `json:"documents"`
	Error           string           `json:"error"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      time.Time        `json:"finished_at,omitzero"`
}

// ReindexChunk is one bounded-batch input to the embedding worker.
type ReindexChunk struct {
	ID   int64
	Text string
}

const reindexJobColumns = `id, profile, status, total_chunks, completed_chunks,
	documents, error, started_at, finished_at`

func (s *Store) migrateEmbeddings(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS embedding_profile (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	profile TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS embedding_reindex_jobs (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	profile          TEXT NOT NULL,
	status           TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
	total_chunks     INTEGER NOT NULL CHECK (total_chunks >= 0),
	completed_chunks INTEGER NOT NULL DEFAULT 0 CHECK (completed_chunks >= 0 AND completed_chunks <= total_chunks),
	documents        INTEGER NOT NULL CHECK (documents >= 0),
	error            TEXT NOT NULL DEFAULT '',
	started_at       TEXT NOT NULL,
	finished_at      TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_embedding_reindex_running
	ON embedding_reindex_jobs(status) WHERE status = 'running';
CREATE TABLE IF NOT EXISTS embedding_reindex_staging (
	job_id    INTEGER NOT NULL REFERENCES embedding_reindex_jobs(id) ON DELETE CASCADE,
	chunk_id  INTEGER NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
	embedding BLOB NOT NULL,
	PRIMARY KEY (job_id, chunk_id)
);
`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // No-op after a successful commit.
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE embedding_reindex_jobs SET status = 'failed', error = ?, finished_at = ?
		 WHERE status = 'running'`,
		"reindex interrupted by process restart", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_reindex_staging`); err != nil {
		return err
	}
	return tx.Commit()
}

// StartReindex waits for admitted mutations, then snapshots the corpus and
// persists the single running job before admitting any further mutations.
// Dimensions are learned from this job's first batch, not a prior index.
func (s *Store) StartReindex(ctx context.Context, target EmbeddingProfile) (ReindexJob, error) {
	if err := ctx.Err(); err != nil {
		return ReindexJob{}, err
	}
	s.corpusMu.Lock()
	defer s.corpusMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReindexJob{}, err
	}
	defer func() { _ = tx.Rollback() }() // No-op after a successful commit.
	var running bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM embedding_reindex_jobs WHERE status = 'running')`).Scan(&running); err != nil {
		return ReindexJob{}, err
	}
	if running {
		return ReindexJob{}, ErrReindexInProgress
	}
	target.Dimensions = 0
	job := ReindexJob{
		Profile: target, Status: ReindexRunning, StartedAt: time.Now().UTC(),
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM chunks)`,
	).Scan(&job.Documents, &job.TotalChunks); err != nil {
		return ReindexJob{}, err
	}
	encoded, err := json.Marshal(job.Profile)
	if err != nil {
		return ReindexJob{}, fmt.Errorf("encode reindex profile: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO embedding_reindex_jobs (profile, status, total_chunks, documents, started_at)
		 VALUES (?, 'running', ?, ?, ?)`,
		string(encoded), job.TotalChunks, job.Documents, job.StartedAt.Format(time.RFC3339Nano))
	if err != nil {
		return ReindexJob{}, err
	}
	if job.ID, err = result.LastInsertId(); err != nil {
		return ReindexJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReindexJob{}, err
	}
	return job, nil
}

// LatestReindex returns the latest job, whether running or finished.
func (s *Store) LatestReindex(ctx context.Context) (ReindexJob, bool, error) {
	job, err := scanReindexJob(s.db.QueryRowContext(ctx,
		`SELECT `+reindexJobColumns+` FROM embedding_reindex_jobs ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return ReindexJob{}, false, nil
	}
	return job, err == nil, err
}

// ReindexJob loads the persisted target rather than trusting a worker's copy.
func (s *Store) ReindexJob(ctx context.Context, id int64) (ReindexJob, error) {
	return scanReindexJob(s.db.QueryRowContext(ctx,
		`SELECT `+reindexJobColumns+` FROM embedding_reindex_jobs WHERE id = ?`, id))
}

type reindexRow interface {
	Scan(...any) error
}

func scanReindexJob(row reindexRow) (ReindexJob, error) {
	var job ReindexJob
	var encoded, started string
	var finished sql.NullString
	if err := row.Scan(&job.ID, &encoded, &job.Status, &job.TotalChunks, &job.CompletedChunks,
		&job.Documents, &job.Error, &started, &finished); err != nil {
		return ReindexJob{}, err
	}
	if err := json.Unmarshal([]byte(encoded), &job.Profile); err != nil {
		return ReindexJob{}, fmt.Errorf("decode reindex profile: %w", err)
	}
	var err error
	if job.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return ReindexJob{}, fmt.Errorf("decode reindex start time: %w", err)
	}
	if finished.Valid {
		if job.FinishedAt, err = time.Parse(time.RFC3339Nano, finished.String); err != nil {
			return ReindexJob{}, fmt.Errorf("decode reindex finish time: %w", err)
		}
	}
	return job, nil
}

func runningReindexJob(ctx context.Context, tx *sql.Tx, id int64) (ReindexJob, error) {
	job, err := scanReindexJob(tx.QueryRowContext(ctx,
		`SELECT `+reindexJobColumns+` FROM embedding_reindex_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && job.Status != ReindexRunning) {
		return ReindexJob{}, ErrReindexNotRunning
	}
	return job, err
}

// ReindexChunkBatch loads at most 16 texts and releases the connection before
// returning, so the embedding callback can use the single-connection database.
func (s *Store) ReindexChunkBatch(ctx context.Context, jobID, afterID int64, limit int) ([]ReindexChunk, error) {
	if afterID < 0 || limit <= 0 || limit > maxReindexBatchSize {
		return nil, fmt.Errorf("invalid reindex batch range")
	}
	job, err := s.ReindexJob(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && job.Status != ReindexRunning) {
		return nil, ErrReindexNotRunning
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, text FROM chunks WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // Release rows on scan errors as well.
	chunks := make([]ReindexChunk, 0, limit)
	for rows.Next() {
		var chunk ReindexChunk
		if err := rows.Scan(&chunk.ID, &chunk.Text); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

// StageReindexBatch validates and persists vectors and progress together,
// without changing the active index. A failed batch leaves no partial staging.
func (s *Store) StageReindexBatch(ctx context.Context, jobID int64, chunks []ReindexChunk, vectors [][]float32) error {
	if len(chunks) == 0 || len(chunks) > maxReindexBatchSize {
		return fmt.Errorf("invalid reindex batch size: %d", len(chunks))
	}
	if len(vectors) != len(chunks) {
		return fmt.Errorf("invalid reindex chunk/embedding count: %d/%d", len(chunks), len(vectors))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // No-op after a successful commit.
	job, err := runningReindexJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	dimensions := job.Profile.Dimensions
	for _, vector := range vectors {
		if err := validateReindexVector(vector, dimensions); err != nil {
			return err
		}
		dimensions = len(vector)
	}
	if job.CompletedChunks+len(chunks) > job.TotalChunks {
		return fmt.Errorf("reindex staging exceeds the corpus snapshot")
	}
	for i := 0; i < len(chunks) && i < len(vectors); i++ {
		chunk := chunks[i]
		result, err := tx.ExecContext(ctx,
			`INSERT INTO embedding_reindex_staging (job_id, chunk_id, embedding)
			 SELECT ?, id, ? FROM chunks WHERE id = ? AND text = ?`,
			jobID, encodeEmbedding(vectors[i]), chunk.ID, chunk.Text)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted != 1 {
			return fmt.Errorf("reindex chunk %d changed or disappeared", chunk.ID)
		}
	}
	job.Profile.Dimensions = dimensions
	encoded, err := json.Marshal(job.Profile)
	if err != nil {
		return fmt.Errorf("encode reindex profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE embedding_reindex_jobs SET profile = ?, completed_chunks = completed_chunks + ? WHERE id = ?`,
		string(encoded), len(chunks), jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func validateReindexVector(vector []float32, dimensions int) error {
	if len(vector) == 0 {
		return fmt.Errorf("reindex returned an empty embedding")
	}
	if dimensions < 0 || (dimensions != 0 && len(vector) != dimensions) {
		return fmt.Errorf("reindex embedding dimensions differ: got %d, want %d", len(vector), dimensions)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("reindex embedding contains a non-finite value")
		}
	}
	return nil
}

// FinishReindex atomically publishes only a complete, valid staged index. It
// waits for readers before opening its transaction; inference never holds one.
// On error the worker must call FailReindex to release the running job.
func (s *Store) FinishReindex(ctx context.Context, jobID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.corpusMu.Lock()
	defer s.corpusMu.Unlock()
	s.embeddingMu.Lock()
	defer s.embeddingMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // No-op after a successful commit.
	job, err := runningReindexJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	var documents, chunks, staged int
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM chunks),
		 (SELECT COUNT(*) FROM embedding_reindex_staging WHERE job_id = ?)`, jobID,
	).Scan(&documents, &chunks, &staged); err != nil {
		return err
	}
	if documents != job.Documents || chunks != job.TotalChunks ||
		staged != chunks || job.CompletedChunks != chunks {
		return fmt.Errorf("reindex does not exactly cover the corpus snapshot")
	}
	if chunks > 0 && job.Profile.Dimensions <= 0 {
		return fmt.Errorf("reindex embedding dimensions are unknown")
	}
	if err := validateStagedReindex(ctx, tx, job); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE chunks SET embedding = (
			SELECT embedding FROM embedding_reindex_staging WHERE job_id = ? AND chunk_id = chunks.id
		 )`, jobID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != int64(chunks) {
		return fmt.Errorf("reindex publication did not update every chunk")
	}
	if err := writeEmbeddingProfile(ctx, tx, job.Profile); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE embedding_reindex_jobs SET status = 'succeeded', finished_at = ?, error = '' WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_reindex_staging WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func validateStagedReindex(ctx context.Context, tx *sql.Tx, job ReindexJob) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.id, stage.embedding FROM chunks c
		 LEFT JOIN embedding_reindex_staging stage ON stage.job_id = ? AND stage.chunk_id = c.id`, job.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }() // Release rows on validation errors as well.
	var blob sql.RawBytes
	var vector []float32
	for rows.Next() {
		var chunkID int64
		if err := rows.Scan(&chunkID, &blob); err != nil {
			return err
		}
		if len(blob)%4 != 0 {
			return fmt.Errorf("reindex chunk %d has an invalid embedding encoding", chunkID)
		}
		vector = decodeEmbeddingInto(vector, blob)
		if err := validateReindexVector(vector, job.Profile.Dimensions); err != nil {
			return fmt.Errorf("reindex chunk %d: %w", chunkID, err)
		}
	}
	return rows.Err()
}

// FailReindex records a terminal failure and clears only that job's staging.
// It never changes the active profile or vectors, or rewrites a finished job.
func (s *Store) FailReindex(ctx context.Context, jobID int64, cause error) error {
	message := "embedding reindex failed"
	if cause != nil {
		message = cause.Error()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // No-op after a successful commit.
	result, err := tx.ExecContext(ctx,
		`UPDATE embedding_reindex_jobs SET status = 'failed', error = ?, finished_at = ?
		 WHERE id = ? AND status = 'running'`,
		message, time.Now().UTC().Format(time.RFC3339Nano), jobID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_reindex_staging WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return tx.Commit()
}
