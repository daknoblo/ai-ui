package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const serverResourceID = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test/providers/Microsoft.CognitiveServices/accounts/test"

type serverFoundrySource struct {
	snapshot   foundry.Snapshot
	fail       bool
	refreshes  int
	images     []foundry.Deployment
	imageErr   error
	imageReads atomic.Int64
}

func (s *serverFoundrySource) ImageModels(context.Context, string) ([]foundry.Deployment, error) {
	s.imageReads.Add(1)
	return s.images, s.imageErr
}

func (s *serverFoundrySource) Refresh(context.Context, string) (foundry.Snapshot, error) {
	s.refreshes++
	if s.fail {
		return foundry.Snapshot{}, errors.New("discovery permission denied")
	}
	return s.snapshot, nil
}

func (*serverFoundrySource) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer test-identity")
	req.Header.Del("api-key")
	return nil
}

func newFoundryTestServer(t *testing.T, language string) (*Server, http.Handler, *serverFoundrySource, *atomic.Int64) {
	t.Helper()
	return newFoundryTestServerWithOverrides(t, language, config.Overrides{})
}

func newFoundryTestServerWithOverrides(t *testing.T, language string, overrides config.Overrides) (*Server, http.Handler, *serverFoundrySource, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-identity" || r.Header.Get("api-key") != "" {
			t.Error("inference did not use only the configured identity")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			var request struct {
				Input []string `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode embeddings: %v", err)
				http.Error(w, "bad input", http.StatusBadRequest)
				return
			}
			type vector struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}
			output := struct {
				Data []vector `json:"data"`
			}{}
			for i := range request.Input {
				output.Data = append(output.Data, vector{Index: i, Embedding: []float32{0.25, 0.75}})
			}
			if err := json.NewEncoder(w).Encode(output); err != nil {
				t.Errorf("write embeddings: %v", err)
			}
			return
		}
		if strings.Contains(r.URL.Path, "/images/") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"prompt is required"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	t.Cleanup(azure.Close)
	srv, handler := newConfiguredServer(t, language, config.Keys{API: "legacy-unused"}, overrides, nil)
	source := &serverFoundrySource{snapshot: foundry.Snapshot{
		ResourceID: serverResourceID, Endpoint: azure.URL + "/openai/v1", RefreshedAt: time.Now(),
		Deployments: []foundry.Deployment{
			{Name: "chat-prod", ModelName: "gpt-4o", ModelVersion: "2024-08-06", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			{Name: "vectors-prod", ModelName: "text-embedding-3-small", ModelVersion: "1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "Standard"},
			{Name: "pictures-prod", ModelName: "gpt-image-1", ModelVersion: "1", ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "GlobalStandard"},
			{Name: "unknown-prod", ModelName: "future-native-model", ModelFormat: "Unknown", ProvisioningState: "Succeeded", SKU: "Standard"},
		},
	}}
	srv.cfg.ConfigureFoundry(serverResourceID, source, nil)
	if err := srv.cfg.SetCatalog(source.snapshot); err != nil {
		t.Fatal(err)
	}
	return srv, handler, source, calls
}

func postFoundryForm(handler http.Handler, path string, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}

func TestFoundryRefreshIsMetadataOnly(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			srv, handler, source, calls := newFoundryTestServer(t, language)
			rec := postFoundryForm(handler, "/config/deployments/refresh", nil)
			if rec.Code != http.StatusOK || source.refreshes != 1 || calls.Load() != 0 {
				t.Fatalf("refresh status=%d metadata=%d inference=%d", rec.Code, source.refreshes, calls.Load())
			}
			for _, expected := range []string{"chat-prod", "gpt-4o", "vectors-prod", "pictures-prod", "unknown-prod", "vision_deployment"} {
				if !strings.Contains(rec.Body.String(), expected) {
					t.Errorf("inventory settings omit %q", expected)
				}
			}
			if strings.Contains(rec.Body.String(), `hx-post="/verify" hx-trigger="load"`) {
				t.Error("refresh rendering automatically triggers inference verification")
			}
			cached, ok, err := srv.store.LoadCatalog(t.Context(), serverResourceID)
			if err != nil || !ok || len(cached.Deployments) != 4 {
				t.Fatalf("inventory was not persisted: ok=%v err=%v", ok, err)
			}
			source.fail = true
			rec = postFoundryForm(handler, "/config/deployments/refresh", nil)
			if !strings.Contains(rec.Body.String(), "discovery permission denied") ||
				len(srv.cfg.FoundryStatus().Catalog.Deployments) != 4 {
				t.Fatal("failed refresh erased the catalog or hid the error")
			}
		})
	}
}

func TestFoundryRoleSaveAndIndependentChatReadiness(t *testing.T) {
	srv, handler, _, _ := newFoundryTestServer(t, "en")
	rec := postFoundryForm(handler, "/config", url.Values{
		"language": {"en"}, "chat_deployment": {"chat-prod"},
	})
	if rec.Code != http.StatusOK || srv.cfg.Get().ChatDeployment != "chat-prod" {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Body.String())
	}
	srv.runChecks(t.Context(), false)
	if !srv.ready.verified() || srv.ready.uploadsAllowed() {
		t.Fatal("optional embeddings must not block chat or enable uploads")
	}
	if err := srv.refreshFoundry(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !srv.ready.verified() {
		t.Fatal("an unchanged metadata refresh invalidated a healthy connection")
	}
	rec = postFoundryForm(handler, "/config", url.Values{
		"language": {"en"}, "chat_deployment": {"vectors-prod"},
	})
	if srv.cfg.Get().ChatDeployment != "chat-prod" || !strings.Contains(rec.Body.String(), "not usable") {
		t.Fatal("a deployment was saved for the wrong operation")
	}

}

func TestFoundryVerificationCannotRestoreStaleReadiness(t *testing.T) {
	ready := &readiness{}
	generation := ready.epoch()
	ready.invalidate()
	if ready.finish(generation, true, true, true, false, nil, false) {
		t.Fatal("a check started before a configuration change overwrote readiness")
	}
	if ready.snapshot().Checked {
		t.Fatal("stale checks were published")
	}
	if !ready.finish(ready.epoch(), true, true, false, true, nil, false) || !ready.verified() || ready.uploadsAllowed() {
		t.Fatal("chat-only readiness is inconsistent")
	}
}

func TestFoundryReindexPollingKeepsTheSettingsDialog(t *testing.T) {
	srv, _, _, _ := newFoundryTestServer(t, "en")
	html := srv.renderString("embedding-index", embeddingIndexView{Enabled: true, Running: true})
	for _, attribute := range []string{`hx-get="/config/embeddings/status"`, `hx-target="this"`, `hx-swap="outerHTML"`} {
		if !strings.Contains(html, attribute) {
			t.Errorf("reindex polling must replace its own fragment, missing %s", attribute)
		}
	}
}

func TestFoundryReindexRequiresConsentAndPreservesText(t *testing.T) {
	srv, handler, _, _ := newFoundryTestServer(t, "en")
	chatID, err := srv.store.CreateChat(t.Context(), "documents", "chat-prod", "auto")
	if err != nil {
		t.Fatal(err)
	}
	documentID, err := srv.store.CreateDocument(t.Context(), chatID, "example.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddChunks(t.Context(), documentID, []string{"first", "second"},
		[][]float32{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	rec := postFoundryForm(handler, "/config", url.Values{
		"language": {"en"}, "chat_deployment": {"chat-prod"}, "embedding_deployment": {"vectors-prod"},
	})
	if !strings.Contains(rec.Body.String(), "usage charges") {
		t.Fatal("model change did not show the cost/consent warning")
	}
	rec = postFoundryForm(handler, "/config/embeddings/reindex", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatal("unconfirmed reindex accepted")
	}
	if _, known, err := srv.store.ActiveEmbeddingProfile(t.Context()); err != nil || known {
		t.Fatalf("legacy vectors were silently assigned a model: known=%v err=%v", known, err)
	}
	rec = postFoundryForm(handler, "/config/embeddings/reindex", url.Values{
		"confirm_reindex": {"yes"}, "reindex_target": {"stale"},
	})
	if !strings.Contains(rec.Body.String(), "confirm again") {
		t.Fatal("stale confirmation was accepted")
	}
	target, err := srv.llm.ConfiguredEmbeddingProfile()
	if err != nil {
		t.Fatal(err)
	}
	rec = postFoundryForm(handler, "/config/embeddings/reindex", url.Values{
		"confirm_reindex": {"yes"}, "reindex_target": {embeddingTarget(target)},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("start reindex: %d", rec.Code)
	}
	srv.jobs.Wait()
	job, exists, err := srv.store.LatestReindex(t.Context())
	if err != nil || !exists || job.Status != "succeeded" {
		t.Fatalf("reindex did not finish: %+v %v", job, err)
	}
	profile, known, err := srv.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known || !profile.SameIdentity(target) || profile.Dimensions != 2 {
		t.Fatalf("wrong active profile: %+v known=%v err=%v", profile, known, err)
	}
	var ids []int64
	if err := srv.store.EachChunkVector(t.Context(), chatID, func(chunk storage.ChunkVector) error {
		ids = append(ids, chunk.ID)
		if len(chunk.Embedding) != 2 || chunk.Embedding[0] != 0.25 {
			t.Error("new embedding not activated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	texts, err := srv.store.ChunkTexts(t.Context(), ids)
	if err != nil || len(texts) != 2 || texts[ids[0]] != "first" || texts[ids[1]] != "second" {
		t.Fatalf("reindex modified document text: %v %v", texts, err)
	}
	view := srv.embeddingIndexData(t.Context())
	if !view.CanReindex || view.NeedsReindex {
		t.Fatal("the active profile must support an optional confirmed repair")
	}
	html := srv.renderString("embedding-index", view)
	if !strings.Contains(html, `name="confirm_reindex"`) || strings.Contains(html, "<details open") {
		t.Fatal("optional repair must retain consent and remain collapsed for a healthy index")
	}
}

func TestFoundryLegacyVectorsCannotBeQueriedSilently(t *testing.T) {
	srv, _, _, calls := newFoundryTestServer(t, "en")
	cfg := srv.cfg.Get()
	cfg.ChatDeployment, cfg.EmbeddingDeployment = "chat-prod", "vectors-prod"
	if err := srv.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	chatID, err := srv.store.CreateChat(t.Context(), "test", "chat-prod", "auto")
	if err != nil {
		t.Fatal(err)
	}
	docID, err := srv.store.CreateDocument(t.Context(), chatID, "legacy.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddChunks(t.Context(), docID, []string{"old text"}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	_, err = srv.buildLLMMessages(t.Context(), chatID, cfg, nil, "question", false)
	if !errors.Is(err, storage.ErrReindexRequired) || calls.Load() != 0 {
		t.Fatalf("unknown vector provenance was not surfaced before inference: %v", err)
	}
}

func TestFoundryUnavailableImageModeDoesNotBecomeChat(t *testing.T) {
	srv, handler, _, _ := newFoundryTestServer(t, "en")
	cfg := srv.cfg.Get()
	cfg.ChatDeployment = "chat-prod"
	if err := srv.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	id, err := srv.store.CreateChat(t.Context(), "image", "chat-prod", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateChatMode(t.Context(), id, storage.ChatModeImage); err != nil {
		t.Fatal(err)
	}
	rec := postFoundryForm(handler, fmt.Sprintf("/chat/%d/send", id), url.Values{
		"message": {"draw an image"}, "mode": {"image"},
	})
	streamURL := submittedGenerationURL(t, rec)
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, httptest.NewRequest(http.MethodGet, streamURL, nil))
	if !strings.Contains(stream.Body.String(), srv.t("stream.image_not_configured")) {
		t.Fatalf("unavailable image requests must reach the explicit image error, not chat: %s", stream.Body.String())
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
	if !strings.Contains(page.Body.String(), `data-mode="chat"`) {
		t.Fatal("an existing image chat has no control to return to chat mode")
	}
}

func TestFoundryShutdownCancelsRequestWork(t *testing.T) {
	srv, _, _, _ := newFoundryTestServer(t, "en")
	started, finished := make(chan struct{}), make(chan struct{})
	handler := srv.requestContext(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	}))
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-started
	srv.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown left inference work holding the active embedding profile")
	}
}
