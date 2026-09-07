package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func embeddingTestProfile() EmbeddingProfile {
	return EmbeddingProfile{
		ResourceID: "/subscriptions/test/accounts/old", Endpoint: "https://old.example/openai/v1",
		Deployment: "old-vectors", ModelName: "old-model", ModelVersion: "1", Dimensions: 2,
	}
}

func embeddingTestTarget() EmbeddingProfile {
	return EmbeddingProfile{
		ResourceID: "/subscriptions/test/accounts/new", Endpoint: "https://new.example/openai/v1",
		Deployment: "new-vectors", ModelName: "new-model", ModelVersion: "2", Dimensions: 2,
	}
}

func seedEmbeddingTest(t *testing.T, known bool) (*Store, int64, int64) {
	t.Helper()
	store := newTestStore(t)
	ctx := t.Context()
	if known {
		if err := store.SetInitialEmbeddingProfile(ctx, embeddingTestProfile()); err != nil {
			t.Fatal(err)
		}
	}
	chatID, err := store.CreateChat(ctx, "embedding test", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	docID, err := store.CreateDocument(ctx, chatID, "test.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddChunks(ctx, docID, []string{"first", "second"}, [][]float32{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	return store, chatID, docID
}

func embeddingTestVectors(ctx context.Context, store *Store, chatID int64) (map[int64][]float32, error) {
	vectors := make(map[int64][]float32)
	err := store.EachChunkVector(ctx, chatID, func(chunk ChunkVector) error {
		vectors[chunk.ID] = append([]float32(nil), chunk.Embedding...)
		return nil
	})
	return vectors, err
}

func assertEmbeddingTestIndex(t *testing.T, store *Store, chatID int64, known bool, want map[int64][]float32) {
	t.Helper()
	profile, exists, err := store.ActiveEmbeddingProfile(t.Context())
	if err != nil || exists != known || (known && profile != embeddingTestProfile()) {
		t.Fatalf("active profile = %+v, %v, %v; want old profile known=%v", profile, exists, err, known)
	}
	got, err := embeddingTestVectors(t.Context(), store, chatID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("active vectors = %v, %v; want %v", got, err, want)
	}
}

func stagedEmbeddingTestCount(t *testing.T, store *Store, jobID int64) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM embedding_reindex_staging WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestEmbeddingProfileIdentity(t *testing.T) {
	profile := embeddingTestProfile()
	tests := map[string]func(*EmbeddingProfile){
		"resource":   func(p *EmbeddingProfile) { p.ResourceID += "/other" },
		"endpoint":   func(p *EmbeddingProfile) { p.Endpoint += "/other" },
		"deployment": func(p *EmbeddingProfile) { p.Deployment += "-other" },
		"model":      func(p *EmbeddingProfile) { p.ModelName += "-other" },
		"version":    func(p *EmbeddingProfile) { p.ModelVersion += "-other" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			other := profile
			change(&other)
			if profile.SameIdentity(other) || other.SameIdentity(profile) {
				t.Fatal("equal dimensions must not make different model identities compatible")
			}
		})
	}
	other := profile
	other.Dimensions++
	if !profile.SameIdentity(other) {
		t.Fatal("dimensions must be checked separately from model identity")
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"resource_id", "endpoint", "deployment", "model_name", "model_version", "dimensions"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing JSON field %q", key)
		}
	}
}

func TestEmbeddingInitialProfileRequiresEmptyIndex(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if _, known, err := store.ActiveEmbeddingProfile(ctx); err != nil || known {
		t.Fatalf("fresh profile: known=%v, err=%v", known, err)
	}
	if _, exists, err := store.LatestReindex(ctx); err != nil || exists {
		t.Fatalf("fresh job: exists=%v, err=%v", exists, err)
	}
	if docs, chunks, err := store.EmbeddingCounts(ctx); err != nil || docs != 0 || chunks != 0 {
		t.Fatalf("fresh counts = %d/%d, %v", docs, chunks, err)
	}
	if err := store.SetInitialEmbeddingProfile(ctx, embeddingTestProfile()); err != nil {
		t.Fatal(err)
	}
	if got, known, err := store.ActiveEmbeddingProfile(ctx); err != nil || !known || got != embeddingTestProfile() {
		t.Fatalf("initial profile = %+v, %v, %v", got, known, err)
	}
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%v", known), func(t *testing.T) {
			store, chatID, _ := seedEmbeddingTest(t, known)
			old, err := embeddingTestVectors(t.Context(), store, chatID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetInitialEmbeddingProfile(t.Context(), embeddingTestTarget()); !errors.Is(err, ErrReindexRequired) {
				t.Fatalf("profile replacement error = %v", err)
			}
			assertEmbeddingTestIndex(t, store, chatID, known, old)
		})
	}
}

func TestEmbeddingReindexSuccessfulSwitch(t *testing.T) {
	store, chatID, _ := seedEmbeddingTest(t, true)
	ctx := t.Context()
	old, err := embeddingTestVectors(ctx, store, chatID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.StartReindex(ctx, embeddingTestTarget())
	if err != nil {
		t.Fatal(err)
	}
	if job.ID <= 0 || job.Status != ReindexRunning || job.TotalChunks != 2 || job.Documents != 1 ||
		job.CompletedChunks != 0 || job.StartedAt.IsZero() || !job.FinishedAt.IsZero() || job.Profile.Dimensions != 0 {
		t.Fatalf("started job = %+v", job)
	}
	batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 16)
	if err != nil || len(batch) != 2 {
		t.Fatalf("batch = %v, %v", batch, err)
	}
	vectors := [][]float32{{0.6, 0.8}, {0.8, 0.6}}
	if err := store.StageReindexBatch(ctx, job.ID, batch, vectors); err != nil {
		t.Fatal(err)
	}
	progress, exists, err := store.LatestReindex(ctx)
	if err != nil || !exists || progress.CompletedChunks != 2 || progress.Profile.Dimensions != 2 ||
		progress.Status != ReindexRunning || !progress.FinishedAt.IsZero() {
		t.Fatalf("progress = %+v, %v, %v", progress, exists, err)
	}
	assertEmbeddingTestIndex(t, store, chatID, true, old)
	if err := store.FinishReindex(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	profile, known, err := store.ActiveEmbeddingProfile(ctx)
	if err != nil || !known || profile != embeddingTestTarget() {
		t.Fatalf("published profile = %+v, %v, %v", profile, known, err)
	}
	got, err := embeddingTestVectors(ctx, store, chatID)
	want := map[int64][]float32{batch[0].ID: vectors[0], batch[1].ID: vectors[1]}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("published vectors = %v, %v; want %v", got, err, want)
	}
	texts, err := store.ChunkTexts(ctx, []int64{batch[0].ID, batch[1].ID})
	if err != nil || texts[batch[0].ID] != "first" || texts[batch[1].ID] != "second" {
		t.Fatalf("chunk texts changed: %v, %v", texts, err)
	}
	done, err := store.ReindexJob(ctx, job.ID)
	if err != nil || done.Status != ReindexSucceeded || done.CompletedChunks != done.TotalChunks ||
		done.FinishedAt.Before(done.StartedAt) || done.Error != "" || stagedEmbeddingTestCount(t, store, job.ID) != 0 {
		t.Fatalf("finished job = %+v, %v", done, err)
	}
	if err := store.FailReindex(ctx, job.ID, errors.New("late failure")); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.ReindexJob(ctx, job.ID)
	if err != nil || unchanged != done {
		t.Fatalf("late failure rewrote completed job = %+v, %v", unchanged, err)
	}
	if err := store.StageReindexBatch(ctx, job.ID, batch, vectors); !errors.Is(err, ErrReindexNotRunning) {
		t.Fatalf("staging into finished job: %v", err)
	}
}

func TestEmbeddingReindexEmptyCorpus(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	job, err := store.StartReindex(ctx, embeddingTestTarget())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialEmbeddingProfile(ctx, embeddingTestProfile()); !errors.Is(err, ErrReindexInProgress) {
		t.Fatalf("initial profile while reindex is running: %v", err)
	}
	if err := store.FinishReindex(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, known, err := store.ActiveEmbeddingProfile(ctx)
	if err != nil || !known || !got.SameIdentity(embeddingTestTarget()) || got.Dimensions != 0 {
		t.Fatalf("empty corpus profile = %+v, %v, %v", got, known, err)
	}
}

func TestEmbeddingReindexInvalidBatchPreservesIndex(t *testing.T) {
	tests := map[string][][]float32{
		"no vectors":           nil,
		"wrong count":          {{1, 2}},
		"empty vector":         {{1, 2}, {}},
		"inconsistent lengths": {{1, 2}, {1, 2, 3}},
		"not a number":         {{1, 2}, {float32(math.NaN()), 2}},
		"positive infinity":    {{1, 2}, {float32(math.Inf(1)), 2}},
		"negative infinity":    {{1, 2}, {float32(math.Inf(-1)), 2}},
	}
	for name, vectors := range tests {
		t.Run(name, func(t *testing.T) {
			store, chatID, _ := seedEmbeddingTest(t, true)
			ctx := t.Context()
			old, err := embeddingTestVectors(ctx, store, chatID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.StartReindex(ctx, embeddingTestTarget())
			if err != nil {
				t.Fatal(err)
			}
			batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 16)
			if err != nil {
				t.Fatal(err)
			}
			stageErr := store.StageReindexBatch(ctx, job.ID, batch, vectors)
			if stageErr == nil {
				t.Fatal("invalid batch was staged")
			}
			progress, err := store.ReindexJob(ctx, job.ID)
			if err != nil || progress.CompletedChunks != 0 || progress.Profile.Dimensions != 0 ||
				stagedEmbeddingTestCount(t, store, job.ID) != 0 {
				t.Fatalf("failed batch left partial staging: %+v, %v", progress, err)
			}
			if err := store.FailReindex(ctx, job.ID, stageErr); err != nil {
				t.Fatal(err)
			}
			assertEmbeddingTestIndex(t, store, chatID, true, old)
			failed, err := store.ReindexJob(ctx, job.ID)
			if err != nil || failed.Status != ReindexFailed || failed.Error == "" || failed.FinishedAt.IsZero() {
				t.Fatalf("failed job = %+v, %v", failed, err)
			}
		})
	}
}

func TestEmbeddingReindexBatchIsTransactional(t *testing.T) {
	store, chatID, _ := seedEmbeddingTest(t, true)
	ctx := t.Context()
	old, err := embeddingTestVectors(ctx, store, chatID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.StartReindex(ctx, embeddingTestTarget())
	if err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]ReindexChunk(nil), batch...)
	bad[1].Text = "changed after snapshot"
	if err := store.StageReindexBatch(ctx, job.ID, bad, [][]float32{{1, 2}, {3, 4}}); err == nil {
		t.Fatal("changed chunk text was accepted")
	}
	if stagedEmbeddingTestCount(t, store, job.ID) != 0 {
		t.Fatal("an earlier row from a failed batch was committed")
	}
	if err := store.StageReindexBatch(ctx, job.ID, batch[:1], [][]float32{{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if err := store.StageReindexBatch(ctx, job.ID, batch[1:], [][]float32{{1, 2, 3}}); err == nil {
		t.Fatal("dimensions changed between batches")
	}
	if err := store.StageReindexBatch(ctx, job.ID, batch[:1], [][]float32{{1, 2}}); err == nil {
		t.Fatal("duplicate chunk was counted twice")
	}
	progress, err := store.ReindexJob(ctx, job.ID)
	if err != nil || progress.CompletedChunks != 1 || stagedEmbeddingTestCount(t, store, job.ID) != 1 {
		t.Fatalf("progress after failed batches = %+v, %v", progress, err)
	}
	if err := store.FinishReindex(ctx, job.ID); err == nil {
		t.Fatal("incomplete staging was published")
	}
	if err := store.FailReindex(ctx, job.ID, errors.New("batch failed")); err != nil {
		t.Fatal(err)
	}
	if stagedEmbeddingTestCount(t, store, job.ID) != 0 {
		t.Fatal("failed job retained staging")
	}
	assertEmbeddingTestIndex(t, store, chatID, true, old)
}

func TestEmbeddingReindexFinishValidatesCorpusAndVectors(t *testing.T) {
	tests := map[string]func(*testing.T, *Store, ReindexJob, []ReindexChunk, int64){
		"missing staged vector": func(t *testing.T, s *Store, job ReindexJob, batch []ReindexChunk, _ int64) {
			_, err := s.db.ExecContext(t.Context(),
				`DELETE FROM embedding_reindex_staging WHERE job_id = ? AND chunk_id = ?`, job.ID, batch[0].ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"invalid encoding": func(t *testing.T, s *Store, job ReindexJob, batch []ReindexChunk, _ int64) {
			_, err := s.db.ExecContext(t.Context(),
				`UPDATE embedding_reindex_staging SET embedding = ? WHERE job_id = ? AND chunk_id = ?`,
				[]byte{1}, job.ID, batch[0].ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"invalid dimensions": func(t *testing.T, s *Store, job ReindexJob, batch []ReindexChunk, _ int64) {
			_, err := s.db.ExecContext(t.Context(),
				`UPDATE embedding_reindex_staging SET embedding = ? WHERE job_id = ? AND chunk_id = ?`,
				encodeEmbedding([]float32{1}), job.ID, batch[0].ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"non-finite staged vector": func(t *testing.T, s *Store, job ReindexJob, batch []ReindexChunk, _ int64) {
			_, err := s.db.ExecContext(t.Context(),
				`UPDATE embedding_reindex_staging SET embedding = ? WHERE job_id = ? AND chunk_id = ?`,
				encodeEmbedding([]float32{1, float32(math.NaN())}), job.ID, batch[0].ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"extra document": func(t *testing.T, s *Store, _ ReindexJob, _ []ReindexChunk, chatID int64) {
			if _, err := s.CreateDocument(t.Context(), chatID, "late.txt", "text/plain"); err != nil {
				t.Fatal(err)
			}
		},
		"publication rollback": func(t *testing.T, s *Store, _ ReindexJob, _ []ReindexChunk, _ int64) {
			_, err := s.db.ExecContext(t.Context(), `
CREATE TRIGGER fail_embedding_publication BEFORE UPDATE ON embedding_reindex_jobs
WHEN NEW.status = 'succeeded' BEGIN SELECT RAISE(ABORT, 'forced publication failure'); END`)
			if err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			store, chatID, _ := seedEmbeddingTest(t, true)
			ctx := t.Context()
			old, err := embeddingTestVectors(ctx, store, chatID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.StartReindex(ctx, embeddingTestTarget())
			if err != nil {
				t.Fatal(err)
			}
			batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 16)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.StageReindexBatch(ctx, job.ID, batch, [][]float32{{9, 8}, {7, 6}}); err != nil {
				t.Fatal(err)
			}
			change(t, store, job, batch, chatID)
			if err := store.FinishReindex(ctx, job.ID); err == nil {
				t.Fatal("invalid staged index was published")
			}
			assertEmbeddingTestIndex(t, store, chatID, true, old)
			progress, err := store.ReindexJob(ctx, job.ID)
			if err != nil || progress.Status != ReindexRunning {
				t.Fatalf("failed publication changed job state: %+v, %v", progress, err)
			}
		})
	}
}

func TestEmbeddingReindexStartupRecovery(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%v", known), func(t *testing.T) {
			store, chatID, _ := seedEmbeddingTest(t, known)
			ctx := t.Context()
			old, err := embeddingTestVectors(ctx, store, chatID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.StartReindex(ctx, embeddingTestTarget())
			if err != nil {
				t.Fatal(err)
			}
			batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.StageReindexBatch(ctx, job.ID, batch, [][]float32{{5, 6, 7}}); err != nil {
				t.Fatal(err)
			}
			path := store.path
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Errorf("close reopened store: %v", err)
				}
			})
			if err := reopened.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			failed, exists, err := reopened.LatestReindex(ctx)
			if err != nil || !exists || failed.ID != job.ID || failed.Status != ReindexFailed ||
				failed.CompletedChunks != 1 || !strings.Contains(failed.Error, "restart") || failed.FinishedAt.IsZero() {
				t.Fatalf("recovered job = %+v, %v, %v", failed, exists, err)
			}
			if stagedEmbeddingTestCount(t, reopened, job.ID) != 0 {
				t.Fatal("interrupted staging was not cleared")
			}
			assertEmbeddingTestIndex(t, reopened, chatID, known, old)
			if err := reopened.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			again, err := reopened.ReindexJob(ctx, job.ID)
			if err != nil || again != failed {
				t.Fatalf("idempotent migration rewrote job = %+v, %v", again, err)
			}
			var catalogs int
			if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_catalog`).Scan(&catalogs); err != nil {
				t.Fatalf("model catalog was not migrated: %v", err)
			}
			next, err := reopened.StartReindex(ctx, embeddingTestTarget())
			if err != nil || next.ID <= job.ID {
				t.Fatalf("could not start a new job after recovery: %+v, %v", next, err)
			}
		})
	}
}

func TestEmbeddingReindexRejectsChangedChunkSnapshot(t *testing.T) {
	for _, change := range []string{"add", "delete", "replace"} {
		t.Run(change, func(t *testing.T) {
			store, chatID, docID := seedEmbeddingTest(t, true)
			ctx := t.Context()
			job, err := store.StartReindex(ctx, embeddingTestTarget())
			if err != nil {
				t.Fatal(err)
			}
			batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 16)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.StageReindexBatch(ctx, job.ID, batch, [][]float32{{9, 8}, {7, 6}}); err != nil {
				t.Fatal(err)
			}
			// Deliberately bypass mutation admission to exercise the final
			// coverage check, including replacement with unchanged counts.
			if change != "add" {
				if _, err := store.db.ExecContext(ctx, `DELETE FROM chunks WHERE id = ?`, batch[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			if change != "delete" {
				if err := store.AddChunks(ctx, docID, []string{"late chunk"}, [][]float32{{3, 4}}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := embeddingTestVectors(ctx, store, chatID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishReindex(ctx, job.ID); err == nil {
				t.Fatal("changed chunk snapshot was published")
			}
			assertEmbeddingTestIndex(t, store, chatID, true, before)
		})
	}
}

func TestEmbeddingReindexConcurrentStarts(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := store.StartReindex(ctx, embeddingTestTarget())
			results <- err
		}()
	}
	close(start)
	created := 0
	for range callers {
		select {
		case err := <-results:
			if err == nil {
				created++
			} else if !errors.Is(err, ErrReindexInProgress) {
				t.Errorf("concurrent start: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if created != 1 {
		t.Fatalf("concurrent starts created %d jobs, want 1", created)
	}
}

func TestEmbeddingReindexWaitsForCorpusMutations(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := store.SetInitialEmbeddingProfile(ctx, embeddingTestProfile()); err != nil {
		t.Fatal(err)
	}
	chatID, err := store.CreateChat(ctx, "test", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	mutated := make(chan error, 1)
	go func() {
		mutated <- store.WithCorpusMutation(ctx, func() error {
			return store.WithEmbeddingProfile(ctx, func(profile EmbeddingProfile, known bool) error {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				if !known || profile != embeddingTestProfile() {
					return errors.New("mutation lost its active profile")
				}
				docID, err := store.CreateDocument(ctx, chatID, "new.txt", "text/plain")
				if err != nil {
					return err
				}
				return store.AddChunks(ctx, docID, []string{"new"}, [][]float32{{1, 0}})
			})
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	type startResult struct {
		job ReindexJob
		err error
	}
	starting := make(chan struct{})
	started := make(chan startResult, 1)
	go func() {
		close(starting)
		job, err := store.StartReindex(ctx, embeddingTestTarget())
		started <- startResult{job, err}
	}()
	<-starting
	select {
	case result := <-started:
		t.Fatalf("start did not wait for existing mutation: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	if err := <-mutated; err != nil {
		t.Fatal(err)
	}
	result := <-started
	if result.err != nil || result.job.Documents != 1 || result.job.TotalChunks != 1 {
		t.Fatalf("snapshot missed admitted mutation: %+v", result)
	}
	called := false
	err = store.WithCorpusMutation(ctx, func() error { called = true; return nil })
	if !errors.Is(err, ErrReindexInProgress) || called {
		t.Fatalf("mutation admitted during reindex: called=%v, err=%v", called, err)
	}
	if _, err := store.StartReindex(ctx, embeddingTestTarget()); !errors.Is(err, ErrReindexInProgress) {
		t.Fatalf("concurrent job admitted: %v", err)
	}
	if err := store.FailReindex(ctx, result.job.ID, errors.New("stop test job")); err != nil {
		t.Fatal(err)
	}
	if err := store.WithCorpusMutation(ctx, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("mutation not re-enabled after failure: %v", err)
	}
}

func TestEmbeddingReindexWaitsForProfileReaders(t *testing.T) {
	store, chatID, _ := seedEmbeddingTest(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	old, err := embeddingTestVectors(ctx, store, chatID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.StartReindex(ctx, embeddingTestTarget())
	if err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReindexChunkBatch(ctx, job.ID, 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageReindexBatch(ctx, job.ID, batch, [][]float32{{9, 8}, {7, 6}}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	read := make(chan error, 1)
	go func() {
		read <- store.WithEmbeddingProfile(ctx, func(profile EmbeddingProfile, known bool) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if !known || profile != embeddingTestProfile() {
				return errors.New("reader did not pin the old embedding profile")
			}
			vectors, err := embeddingTestVectors(ctx, store, chatID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(vectors, old) {
				return errors.New("reader compared old-profile inference with new vectors")
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	starting := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(starting)
		finished <- store.FinishReindex(ctx, job.ID)
	}()
	<-starting
	select {
	case err := <-finished:
		t.Fatalf("publication did not wait for reader: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, time.Second)
	defer checkCancel()
	if _, _, err := store.EmbeddingCounts(checkCtx); err != nil {
		t.Fatalf("publication held the database while waiting for its index lock: %v", err)
	}
	unblock()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := store.WithEmbeddingProfile(ctx, func(profile EmbeddingProfile, known bool) error {
		if !known || profile != embeddingTestTarget() {
			return errors.New("new reader did not see published profile")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddingReindexContextCancellation(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fn := func() error {
		t.Error("callback ran with a canceled context")
		return nil
	}
	if err := store.WithCorpusMutation(ctx, fn); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation: %v", err)
	}
	if err := store.WithEmbeddingProfile(ctx, func(EmbeddingProfile, bool) error { return fn() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader: %v", err)
	}
	if _, err := store.StartReindex(ctx, embeddingTestTarget()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start: %v", err)
	}
	if _, exists, err := store.LatestReindex(t.Context()); err != nil || exists {
		t.Fatalf("canceled start persisted a job: exists=%v, err=%v", exists, err)
	}
	if err := store.SetInitialEmbeddingProfile(ctx, embeddingTestTarget()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled initial profile: %v", err)
	}
}
