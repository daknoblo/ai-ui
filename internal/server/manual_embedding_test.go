package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/logbuf"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
)

type manualEmbeddingBackend struct {
	url                string
	calls              atomic.Int64
	dims               int
	newDims            int
	fail               atomic.Bool
	newCalls           atomic.Int64
	overrideDimensions atomic.Int64
}

func newManualEmbeddingBackend(t *testing.T, dimensions int) *manualEmbeddingBackend {
	t.Helper()
	backend := &manualEmbeddingBackend{dims: dimensions, newDims: dimensions}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`)) // Test response only.
			return
		}
		backend.calls.Add(1)
		if r.Header.Get("api-key") != "embedding-test-key" || r.Header.Get("Authorization") != "" {
			t.Error("manual embedding request did not use its dedicated API key")
		}
		if backend.fail.Load() {
			http.Error(w, "embedding unavailable", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Model == "new-vectors" {
			backend.newCalls.Add(1)
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			dimensions := backend.dims
			if request.Model == "new-vectors" {
				dimensions = backend.newDims
			}
			if changed := backend.overrideDimensions.Load(); changed > 0 {
				dimensions = int(changed)
			}
			vector := make([]float32, dimensions)
			vector[0] = 1
			data[i] = map[string]any{"index": i, "embedding": vector}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	backend.url = upstream.URL + "/openai/v1"
	return backend
}

func newManualEmbeddingServer(t *testing.T, backend *manualEmbeddingBackend) (*Server, http.Handler, int64) {
	t.Helper()
	srv, handler := newConfiguredServer(t, "en",
		config.Keys{API: "chat-test-key", Embedding: "embedding-test-key"}, config.Overrides{},
		func(cfg *config.Config) {
			cfg.Endpoint, cfg.EmbeddingDeployment, cfg.ChatDeployment = backend.url, "old-vectors", "chat"
		})
	chatID, err := srv.store.CreateChat(t.Context(), "documents", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	return srv, handler, chatID
}

func seedManualEmbedding(t *testing.T, srv *Server, chatID int64, known bool) {
	t.Helper()
	if known {
		if err := srv.verifyActiveEmbedding(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.ingestor.Ingest(t.Context(), chatID, "test.txt", "text/plain", []byte("original document text")); err != nil {
			t.Fatal(err)
		}
		return
	}
	documentID, err := srv.store.CreateDocument(t.Context(), chatID, "legacy.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddChunks(t.Context(), documentID, []string{"legacy document text"}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
}

func TestManualEmbeddingFreshProfileAndLegacyGate(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			backend := newManualEmbeddingBackend(t, 2)
			srv, handler, chatID := newManualEmbeddingServer(t, backend)
			if legacy {
				seedManualEmbedding(t, srv, chatID, false)
			}
			if _, err := srv.ingestor.Ingest(t.Context(), chatID, "blocked.txt", "text/plain", []byte("blocked")); !errors.Is(err, storage.ErrReindexRequired) {
				t.Fatalf("unverified upload: %v", err)
			}
			if legacy {
				if _, err := srv.retriever.Retrieve(t.Context(), chatID, "query", 3); !errors.Is(err, storage.ErrReindexRequired) {
					t.Fatalf("legacy query: %v", err)
				}
				if err := srv.verifyActiveEmbedding(t.Context()); !errors.Is(err, storage.ErrReindexRequired) {
					t.Fatalf("legacy verification: %v", err)
				}
				if backend.calls.Load() != 0 {
					t.Fatal("legacy index triggered inference without rebuild consent")
				}
				if _, known, err := srv.store.ActiveEmbeddingProfile(t.Context()); err != nil || known {
					t.Fatal("legacy vectors were silently relabeled")
				}
			} else {
				seedManualEmbedding(t, srv, chatID, true)
				profile, known, err := srv.store.ActiveEmbeddingProfile(t.Context())
				if err != nil || !known || profile.Endpoint != backend.url || profile.Deployment != "old-vectors" ||
					profile.ResourceID != "" || profile.Dimensions != 2 {
					t.Fatalf("manual profile not persisted: %+v, %v, %v", profile, known, err)
				}
				if results, err := srv.retriever.Retrieve(t.Context(), chatID, "query", 3); err != nil || len(results) != 1 {
					t.Fatalf("manual retrieval: %v, %v", results, err)
				}
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
			if !strings.Contains(rec.Body.String(), `id="embedding-index"`) ||
				!strings.Contains(rec.Body.String(), `name="confirm_reindex"`) {
				t.Fatal("manual settings omit the explicit reindex workflow")
			}
		})
	}
}

func TestManualEmbeddingChangesKeepActiveIndexUntilReindex(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, changeEndpoint := range []bool{false, true} {
			t.Run(fmt.Sprintf("dimensions=%d/endpoint=%v", dimensions, changeEndpoint), func(t *testing.T) {
				old := newManualEmbeddingBackend(t, 2)
				old.newDims = dimensions
				targetBackend := old
				if changeEndpoint {
					targetBackend = newManualEmbeddingBackend(t, dimensions)
				}
				srv, handler, chatID := newManualEmbeddingServer(t, old)
				seedManualEmbedding(t, srv, chatID, true)
				active, _, err := srv.store.ActiveEmbeddingProfile(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				cfg := srv.cfg.Get()
				cfg.EmbeddingEndpoint = targetBackend.url
				if !changeEndpoint {
					cfg.EmbeddingDeployment = "new-vectors"
				}
				if err := srv.cfg.Save(cfg); err != nil {
					t.Fatal(err)
				}
				if err := srv.verifyActiveEmbedding(t.Context()); err != nil {
					t.Fatalf("old profile verification: %v", err)
				}
				view := srv.embeddingIndexData(t.Context())
				if !view.NeedsReindex || !view.CanReindex {
					t.Fatal("manual change did not require a rebuild")
				}
				if results, err := srv.retriever.Retrieve(t.Context(), chatID, "query", 3); err != nil || len(results) != 1 {
					t.Fatalf("old profile retrieval: %v, %v", results, err)
				}
				target, err := srv.llm.ConfiguredEmbeddingProfile()
				if err != nil {
					t.Fatal(err)
				}
				before := targetBackend.calls.Load()
				if (changeEndpoint && before != 0) || (!changeEndpoint && old.newCalls.Load() != 0) {
					t.Fatal("verification/retrieval used the pending target instead of the active profile")
				}
				rec := postFoundryForm(handler, "/config/embeddings/reindex", url.Values{"reindex_target": {embeddingTarget(target)}})
				if rec.Code != http.StatusBadRequest || targetBackend.calls.Load() != before {
					t.Fatal("reindex without cost confirmation was allowed")
				}
				job, err := srv.store.StartReindex(t.Context(), target)
				if err != nil {
					t.Fatal(err)
				}
				if err := rag.Reindex(t.Context(), srv.store, job, func(ctx context.Context, profile storage.EmbeddingProfile, inputs []string) ([][]float32, error) {
					got, known, err := srv.store.ActiveEmbeddingProfile(ctx)
					if err != nil || !known || got != active {
						t.Fatal("staging published the target profile prematurely")
					}
					if results, err := srv.retriever.Retrieve(ctx, chatID, "during staging", 3); err != nil || len(results) != 1 {
						t.Fatalf("retrieval during staging: %v, %v", results, err)
					}
					if _, err := srv.ingestor.Ingest(ctx, chatID, "blocked.txt", "text/plain", []byte("blocked")); !errors.Is(err, storage.ErrReindexInProgress) {
						t.Fatalf("upload during reindex: %v", err)
					}
					return srv.llm.EmbedProfile(ctx, profile, inputs)
				}); err != nil {
					t.Fatal(err)
				}
				got, known, err := srv.store.ActiveEmbeddingProfile(t.Context())
				if err != nil || !known || !got.SameIdentity(target) || got.Dimensions != dimensions {
					t.Fatalf("published profile = %+v, %v, %v", got, known, err)
				}
				if results, err := srv.retriever.Retrieve(t.Context(), chatID, "after rebuild", 3); err != nil || len(results) != 1 {
					t.Fatalf("rebuilt retrieval: %v, %v", results, err)
				}
			})
		}
	}
}

func TestManualReindexFailureAndCancellationPreserveIndex(t *testing.T) {
	for _, cancelJob := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelJob), func(t *testing.T) {
			old := newManualEmbeddingBackend(t, 2)
			targetBackend := newManualEmbeddingBackend(t, 3)
			srv, _, chatID := newManualEmbeddingServer(t, old)
			seedManualEmbedding(t, srv, chatID, true)
			active, _, err := srv.store.ActiveEmbeddingProfile(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			cfg := srv.cfg.Get()
			cfg.EmbeddingEndpoint = targetBackend.url
			if err := srv.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			target, err := srv.llm.ConfiguredEmbeddingProfile()
			if err != nil {
				t.Fatal(err)
			}
			job, err := srv.store.StartReindex(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelJob {
				cancel()
			} else {
				targetBackend.fail.Store(true)
			}
			if err := rag.Reindex(ctx, srv.store, job, srv.llm.EmbedProfile); err == nil {
				t.Fatal("failed worker reported success")
			}
			got, known, err := srv.store.ActiveEmbeddingProfile(t.Context())
			if err != nil || !known || !reflect.DeepEqual(got, active) {
				t.Fatal("failed/canceled reindex replaced the active profile")
			}
			if results, err := srv.retriever.Retrieve(t.Context(), chatID, "still searchable", 3); err != nil || len(results) != 1 {
				t.Fatalf("old retrieval after failure: %v, %v", results, err)
			}
		})
	}
}

func TestManualLegacyReindexRequiresMatchingConfirmedTarget(t *testing.T) {
	backend := newManualEmbeddingBackend(t, 3)
	srv, handler, chatID := newManualEmbeddingServer(t, backend)
	seedManualEmbedding(t, srv, chatID, false)
	target, err := srv.llm.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}

	rec := postFoundryForm(handler, "/config/embeddings/reindex", url.Values{
		"confirm_reindex": {"yes"}, "reindex_target": {"stale"},
	})
	if rec.Code != http.StatusOK || backend.calls.Load() != 0 {
		t.Fatal("stale confirmed target was accepted")
	}
	rec = postFoundryForm(handler, "/config/embeddings/reindex", url.Values{
		"confirm_reindex": {"yes"}, "reindex_target": {embeddingTarget(target)},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed manual rebuild rejected: %s", rec.Body.String())
	}
	srv.jobs.Wait()
	job, known, err := srv.store.LatestReindex(t.Context())
	if err != nil || !known || job.Status != storage.ReindexSucceeded {
		t.Fatalf("manual reindex did not complete: %+v, %v", job, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if results, err := srv.retriever.Retrieve(ctx, chatID, "legacy text", 3); err != nil || len(results) != 1 {
		t.Fatalf("rebuilt legacy retrieval: %v, %v", results, err)
	}
}

func TestManualEmbeddingProfileSurvivesStartup(t *testing.T) {
	for _, changeEndpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed-endpoint=%v", changeEndpoint), func(t *testing.T) {
			backend := newManualEmbeddingBackend(t, 2)
			other := newManualEmbeddingBackend(t, 3)
			dir := t.TempDir()
			configPath, databasePath := filepath.Join(dir, "config.json"), filepath.Join(dir, "data.db")
			newServer := func() *Server {
				t.Helper()
				cfg := config.NewStore(configPath, config.Keys{Embedding: "embedding-test-key"}, config.Overrides{})
				if _, err := cfg.Load(); err != nil {
					t.Fatal(err)
				}
				db, err := storage.Open(databasePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Migrate(t.Context()); err != nil {
					t.Fatal(err)
				}
				srv := New(cfg, db, logbuf.New(10))
				t.Cleanup(func() {
					srv.Close()
					_ = db.Close() // Already closed for the simulated restart.
				})
				return srv
			}
			srv := newServer()
			cfg := srv.cfg.Get()
			cfg.EmbeddingEndpoint, cfg.EmbeddingDeployment = backend.url, "old-vectors"
			if err := srv.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			chatID, err := srv.store.CreateChat(t.Context(), "startup", "", "auto")
			if err != nil {
				t.Fatal(err)
			}
			seedManualEmbedding(t, srv, chatID, true)
			before, _, err := srv.store.ActiveEmbeddingProfile(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			cfg.EmbeddingDeployment = "new-vectors"
			if changeEndpoint {
				cfg.EmbeddingEndpoint = other.url
			}
			if err := srv.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			srv.Close()
			if err := srv.store.Close(); err != nil {
				t.Fatal(err)
			}
			restarted := newServer()
			err = restarted.verifyActiveEmbedding(t.Context())
			if (err != nil) != changeEndpoint {
				t.Fatalf("restart verification = %v, endpoint changed=%v", err, changeEndpoint)
			}
			after, known, err := restarted.store.ActiveEmbeddingProfile(t.Context())
			if err != nil || !known || after != before {
				t.Fatal("restart relabeled an existing manual index")
			}
			target, err := llm.New(restarted.cfg).ConfiguredEmbeddingProfile()
			if err != nil || after.SameIdentity(target) || !restarted.embeddingIndexData(t.Context()).NeedsReindex {
				t.Fatal("restart hid a pending embedding selection")
			}
			if changeEndpoint && other.calls.Load() != 0 {
				t.Fatal("startup queried old vectors with the newly selected endpoint")
			}
		})
	}
}
