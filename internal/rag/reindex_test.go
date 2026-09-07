package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/storage"
)

type reindexFixture struct {
	store   *storage.Store
	chatID  int64
	profile storage.EmbeddingProfile
	vectors map[int64][]float32
}

func newReindexFixture(t *testing.T, count int, known bool) reindexFixture {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "reindex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close reindex store: %v", err)
		}
	})
	ctx := t.Context()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fixture := reindexFixture{
		store: store,
		profile: storage.EmbeddingProfile{
			ResourceID: "/accounts/old", Endpoint: "https://old.example/openai/v1",
			Deployment: "old", ModelName: "old-model", ModelVersion: "1", Dimensions: 2,
		},
	}
	if known {
		if err := store.SetInitialEmbeddingProfile(ctx, fixture.profile); err != nil {
			t.Fatal(err)
		}
	}
	fixture.chatID, err = store.CreateChat(ctx, "reindex", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		docID, err := store.CreateDocument(ctx, fixture.chatID, "test.txt", "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		texts := make([]string, count)
		vectors := make([][]float32, count)
		for i := range count {
			texts[i] = fmt.Sprintf("chunk %d", i)
			vectors[i] = []float32{float32(i + 1), 0}
		}
		if err := store.AddChunks(ctx, docID, texts, vectors); err != nil {
			t.Fatal(err)
		}
	}
	fixture.vectors, err = reindexVectors(ctx, store, fixture.chatID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func reindexTarget() storage.EmbeddingProfile {
	return storage.EmbeddingProfile{
		ResourceID: "/accounts/new", Endpoint: "https://new.example/openai/v1",
		Deployment: "new", ModelName: "new-model", ModelVersion: "2",
	}
}

func reindexVectors(ctx context.Context, store *storage.Store, chatID int64) (map[int64][]float32, error) {
	vectors := make(map[int64][]float32)
	err := store.EachChunkVector(ctx, chatID, func(chunk storage.ChunkVector) error {
		vectors[chunk.ID] = append([]float32(nil), chunk.Embedding...)
		return nil
	})
	return vectors, err
}

func assertReindexUnchanged(t *testing.T, fixture reindexFixture, known bool) {
	t.Helper()
	profile, exists, err := fixture.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || exists != known || (known && profile != fixture.profile) {
		t.Fatalf("old profile changed: %+v, known=%v, err=%v", profile, exists, err)
	}
	vectors, err := reindexVectors(t.Context(), fixture.store, fixture.chatID)
	if err != nil || !reflect.DeepEqual(vectors, fixture.vectors) {
		t.Fatalf("old vectors changed: %v, err=%v", vectors, err)
	}
}

func TestReindexBatchesPublishTogether(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%v", known), func(t *testing.T) {
			const count = 2*embedBatchSize + 3
			fixture := newReindexFixture(t, count, known)
			store := fixture.store
			job, err := store.StartReindex(t.Context(), reindexTarget())
			if err != nil {
				t.Fatal(err)
			}
			calls, completed := 0, 0
			err = Reindex(t.Context(), store, job,
				func(ctx context.Context, profile storage.EmbeddingProfile, inputs []string) ([][]float32, error) {
					// These queries must work with MaxOpenConns(1): neither
					// the read batch nor the worker may retain a transaction.
					checkCtx, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					progress, exists, err := store.LatestReindex(checkCtx)
					if err != nil {
						return nil, fmt.Errorf("read progress between batches: %w", err)
					}
					if !exists || progress.CompletedChunks != completed || progress.Status != storage.ReindexRunning {
						return nil, fmt.Errorf("invalid progress between batches: %+v", progress)
					}
					if !profile.SameIdentity(reindexTarget()) || (calls == 0 && profile.Dimensions != 0) ||
						(calls > 0 && profile.Dimensions != 3) {
						return nil, fmt.Errorf("unexpected embedding profile: %+v", profile)
					}
					if len(inputs) == 0 || len(inputs) > embedBatchSize {
						return nil, fmt.Errorf("unbounded embedding batch: %d", len(inputs))
					}
					if err := store.WithEmbeddingProfile(checkCtx, func(active storage.EmbeddingProfile, exists bool) error {
						if exists != known || (known && active != fixture.profile) {
							return errors.New("staging changed the active profile")
						}
						vectors, err := reindexVectors(checkCtx, store, fixture.chatID)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(vectors, fixture.vectors) {
							return errors.New("staging changed active vectors")
						}
						return nil
					}); err != nil {
						return nil, err
					}
					if err := store.WithCorpusMutation(checkCtx, func() error {
						return errors.New("mutation callback must not run")
					}); !errors.Is(err, storage.ErrReindexInProgress) {
						if err == nil {
							return nil, errors.New("mutation was admitted")
						}
						return nil, fmt.Errorf("mutation was admitted: %w", err)
					}
					vectors := make([][]float32, len(inputs))
					for i, input := range inputs {
						if want := fmt.Sprintf("chunk %d", completed+i); input != want {
							return nil, fmt.Errorf("batch text = %q, want %q", input, want)
						}
						vectors[i] = []float32{0, float32(completed + i + 1), 1}
					}
					calls++
					completed += len(inputs)
					return vectors, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 3 || completed != count {
				t.Fatalf("worker calls/chunks = %d/%d", calls, completed)
			}
			profile, exists, err := store.ActiveEmbeddingProfile(t.Context())
			if err != nil || !exists || !profile.SameIdentity(reindexTarget()) || profile.Dimensions != 3 {
				t.Fatalf("new profile = %+v, %v, %v", profile, exists, err)
			}
			vectors, err := reindexVectors(t.Context(), store, fixture.chatID)
			if err != nil || len(vectors) != count {
				t.Fatalf("new vectors = %v, %v", vectors, err)
			}
			for id, old := range fixture.vectors {
				if want := []float32{0, old[0], 1}; !reflect.DeepEqual(vectors[id], want) {
					t.Errorf("vector %d = %v, want %v", id, vectors[id], want)
				}
			}
			done, err := store.ReindexJob(t.Context(), job.ID)
			if err != nil || done.Status != storage.ReindexSucceeded || done.CompletedChunks != count ||
				done.TotalChunks != count || done.Documents != 1 || done.FinishedAt.IsZero() || done.Error != "" {
				t.Fatalf("completed job = %+v, %v", done, err)
			}
		})
	}
}

func TestReindexInvalidResponsesPreserveOldIndex(t *testing.T) {
	embeddingErr := errors.New("offline embedding failure")
	tests := []struct {
		name    string
		vectors [][]float32
		err     error
	}{
		{name: "embedding error", err: embeddingErr},
		{name: "no vectors"},
		{name: "wrong count", vectors: [][]float32{{1, 2}}},
		{name: "empty vector", vectors: [][]float32{{1, 2}, {}}},
		{name: "inconsistent dimensions", vectors: [][]float32{{1, 2}, {1}}},
		{name: "not a number", vectors: [][]float32{{1, 2}, {1, float32(math.NaN())}}},
		{name: "infinity", vectors: [][]float32{{1, 2}, {1, float32(math.Inf(1))}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReindexFixture(t, 2, true)
			job, err := fixture.store.StartReindex(t.Context(), reindexTarget())
			if err != nil {
				t.Fatal(err)
			}
			err = Reindex(t.Context(), fixture.store, job,
				func(context.Context, storage.EmbeddingProfile, []string) ([][]float32, error) {
					return test.vectors, test.err
				})
			if err == nil || (test.err != nil && !errors.Is(err, test.err)) {
				t.Fatalf("worker error = %v, want failure %v", err, test.err)
			}
			assertReindexUnchanged(t, fixture, true)
			failed, loadErr := fixture.store.ReindexJob(t.Context(), job.ID)
			if loadErr != nil || failed.Status != storage.ReindexFailed || failed.CompletedChunks != 0 ||
				failed.FinishedAt.IsZero() || failed.Error != err.Error() {
				t.Fatalf("failed job = %+v, %v", failed, loadErr)
			}
			if err := fixture.store.WithCorpusMutation(t.Context(), func() error { return nil }); err != nil {
				t.Fatalf("failed worker did not release mutations: %v", err)
			}
		})
	}
}

func TestReindexLaterBatchFailure(t *testing.T) {
	for _, dimensions := range []bool{false, true} {
		t.Run(fmt.Sprintf("dimension-mismatch=%v", dimensions), func(t *testing.T) {
			fixture := newReindexFixture(t, embedBatchSize+1, true)
			job, err := fixture.store.StartReindex(t.Context(), reindexTarget())
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			err = Reindex(t.Context(), fixture.store, job,
				func(_ context.Context, _ storage.EmbeddingProfile, inputs []string) ([][]float32, error) {
					calls++
					if calls > 1 {
						if dimensions {
							return [][]float32{{1, 2, 3}}, nil
						}
						return nil, errors.New("late embedding failure")
					}
					vectors := make([][]float32, len(inputs))
					for i := range vectors {
						vectors[i] = []float32{5, 6}
					}
					return vectors, nil
				})
			if err == nil {
				t.Fatal("later batch failure was ignored")
			}
			assertReindexUnchanged(t, fixture, true)
			failed, err := fixture.store.ReindexJob(t.Context(), job.ID)
			if err != nil || failed.Status != storage.ReindexFailed || failed.CompletedChunks != embedBatchSize {
				t.Fatalf("failed progress = %+v, %v", failed, err)
			}
			next, err := fixture.store.StartReindex(t.Context(), reindexTarget())
			if err != nil || next.ID <= job.ID {
				t.Fatalf("new job after failure = %+v, %v", next, err)
			}
		})
	}
}

func TestReindexCancellationPersistsFailure(t *testing.T) {
	for _, phase := range []string{"before worker", "after embedding", "later batch"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newReindexFixture(t, embedBatchSize+1, false)
			job, err := fixture.store.StartReindex(t.Context(), reindexTarget())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "before worker" {
				cancel()
			}
			calls := 0
			err = Reindex(ctx, fixture.store, job,
				func(_ context.Context, _ storage.EmbeddingProfile, inputs []string) ([][]float32, error) {
					calls++
					if phase == "after embedding" || calls > 1 {
						cancel()
					}
					vectors := make([][]float32, len(inputs))
					for i := range vectors {
						vectors[i] = []float32{5, 6}
					}
					return vectors, nil
				})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
			assertReindexUnchanged(t, fixture, false)
			failed, loadErr := fixture.store.ReindexJob(t.Context(), job.ID)
			wantCompleted := 0
			if phase == "later batch" {
				wantCompleted = embedBatchSize
			}
			if loadErr != nil || failed.Status != storage.ReindexFailed || failed.CompletedChunks != wantCompleted ||
				failed.Error == "" || failed.FinishedAt.IsZero() {
				t.Fatalf("cancellation was not persisted: %+v, %v", failed, loadErr)
			}
			if err := fixture.store.WithCorpusMutation(t.Context(), func() error { return nil }); err != nil {
				t.Fatalf("canceled worker did not release mutations: %v", err)
			}
		})
	}
}

func TestReindexEmptyCorpusNeedsNoInference(t *testing.T) {
	fixture := newReindexFixture(t, 0, false)
	job, err := fixture.store.StartReindex(t.Context(), reindexTarget())
	if err != nil {
		t.Fatal(err)
	}
	if err := Reindex(t.Context(), fixture.store, job, nil); err != nil {
		t.Fatal(err)
	}
	profile, known, err := fixture.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known || !profile.SameIdentity(reindexTarget()) || profile.Dimensions != 0 {
		t.Fatalf("empty index activation = %+v, %v, %v", profile, known, err)
	}
}

func TestReindexUsesPersistedTarget(t *testing.T) {
	fixture := newReindexFixture(t, 1, true)
	job, err := fixture.store.StartReindex(t.Context(), reindexTarget())
	if err != nil {
		t.Fatal(err)
	}
	job.Profile = fixture.profile
	if err := Reindex(t.Context(), fixture.store, job,
		func(_ context.Context, profile storage.EmbeddingProfile, _ []string) ([][]float32, error) {
			if !profile.SameIdentity(reindexTarget()) {
				return nil, errors.New("worker trusted a stale target instead of the persisted job")
			}
			return [][]float32{{5, 6}}, nil
		}); err != nil {
		t.Fatal(err)
	}
}
