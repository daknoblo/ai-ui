package rag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daknoblo/ai-ui/internal/storage"
)

// Reindex embeds a stable corpus in bounded batches, then publishes all vectors
// and their profile atomically. Failed or canceled jobs leave the old index live.
func Reindex(ctx context.Context, store *storage.Store, job storage.ReindexJob,
	embed func(context.Context, storage.EmbeddingProfile, []string) ([][]float32, error),
) (err error) {
	jobID := job.ID
	defer func() {
		if err == nil {
			return
		}
		// Shutdown and request cancellation must not prevent durable failure
		// cleanup, but a broken database must not delay shutdown indefinitely.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if failErr := store.FailReindex(cleanupCtx, jobID, err); failErr != nil {
			err = errors.Join(err, fmt.Errorf("record reindex failure: %w", failErr))
		}
	}()
	job, err = store.ReindexJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load reindex job: %w", err)
	}
	if job.Status != storage.ReindexRunning {
		return storage.ErrReindexNotRunning
	}
	var afterID int64
	for {
		batch, err := store.ReindexChunkBatch(ctx, jobID, afterID, embedBatchSize)
		if err != nil {
			return fmt.Errorf("read reindex batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		if embed == nil {
			return fmt.Errorf("no reindex embedding function supplied")
		}
		texts := make([]string, len(batch))
		for i, chunk := range batch {
			texts[i] = chunk.Text
		}
		vectors, err := embed(ctx, job.Profile, texts)
		if err != nil {
			return fmt.Errorf("embed reindex batch: %w", err)
		}
		if err := store.StageReindexBatch(ctx, jobID, batch, vectors); err != nil {
			return fmt.Errorf("stage reindex batch: %w", err)
		}
		job.Profile.Dimensions = len(vectors[0])
		afterID = batch[len(batch)-1].ID
	}
	if err := store.FinishReindex(ctx, jobID); err != nil {
		return fmt.Errorf("publish reindex: %w", err)
	}
	return nil
}
