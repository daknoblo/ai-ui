package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/llm"
)

const serverImageResourceID = serverResourceID + "-images"

func separateImageTestServer(t *testing.T) (*Server, http.Handler, *serverFoundrySource, *serverFoundrySource, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	s, handler, primary, primaryCalls := newFoundryTestServer(t, "en")
	primary.snapshot.Deployments = slices.DeleteFunc(primary.snapshot.Deployments, func(d foundry.Deployment) bool {
		return d.Supports(foundry.Images)
	})
	primary.images = []foundry.Deployment{{
		Name: "gpt-image-2", ModelName: "gpt-image-2", ModelFormat: "OpenAI", Source: foundry.ModelsAPISource,
	}}
	imageCalls := &atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageCalls.Add(1)
		if r.Header.Get("Authorization") == "" || r.Header.Get("api-key") != "" ||
			r.URL.Query().Get("api-version") != "preview" {
			t.Error("image request did not use the dedicated identity route and preview API")
		}
		if !strings.HasPrefix(r.URL.Path, "/openai/v1/images/") {
			t.Errorf("non-image request reached image resource: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/generations") {
			var body struct{ Model, Prompt string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if body.Model != "gpt-image-2" {
				t.Errorf("unexpected image deployment: %q", body.Model)
			}
			if body.Prompt == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"prompt is required"}}`)) // Local probe fixture.
				return
			}
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`)) // Local generation fixture.
	}))
	t.Cleanup(backend.Close)
	images := &serverFoundrySource{snapshot: foundry.Snapshot{
		ResourceID: serverImageResourceID, Endpoint: backend.URL + "/openai/v1", RefreshedAt: time.Now(),
		Deployments: []foundry.Deployment{{
			Name: "gpt-image-2", ModelName: "gpt-image-2", ModelVersion: "2026-04-21",
			ModelFormat: "OpenAI", ProvisioningState: "Succeeded", SKU: "GlobalStandard",
		}},
	}}
	s.cfg.ConfigureImageFoundry(serverImageResourceID, images, nil)
	cfg := s.cfg.Get()
	cfg.ChatDeployment, cfg.EmbeddingDeployment, cfg.ImageDeployment = "chat-prod", "vectors-prod", "gpt-image-2"
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return s, handler, primary, images, primaryCalls, imageCalls
}

func TestSeparateImageResourceRoutesAndPreservesIndex(t *testing.T) {
	s, handler, primary, images, primaryCalls, imageCalls := separateImageTestServer(t)
	if err := s.refreshFoundry(t.Context()); err != nil {
		t.Fatal(err)
	}
	if primary.imageReads.Load() != 0 || images.imageReads.Load() != 0 {
		t.Fatal("split-resource discovery must not query the global Models API catalog")
	}
	if err := s.verifyActiveEmbedding(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, known, err := s.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known {
		t.Fatalf("missing embedding profile: %v", err)
	}
	count := primaryCalls.Load()
	if err := s.llm.VerifyImage(t.Context(), "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.llm.GenerateImage(t.Context(), "A blue park", llm.ImageOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.llm.EditImage(t.Context(), "Make it green",
		llm.ImageSource{Name: "source.png", MIME: "image/png", Data: []byte("image")}, llm.ImageOptions{}); err != nil {
		t.Fatal(err)
	}
	if imageCalls.Load() != 3 || primaryCalls.Load() != count {
		t.Fatalf("image calls reached the wrong resource: primary=%d image=%d", primaryCalls.Load(), imageCalls.Load())
	}
	if err := s.llm.VerifyChat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.llm.EmbedProfile(t.Context(), before, []string{"query"}); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || before != after {
		t.Fatalf("image setup changed the embedding profile: %+v, %v", after, err)
	}
	view := s.foundryData(t.Context())
	if len(view.ImageChoices) != 1 || view.ImageChoices[0].Name != "gpt-image-2" ||
		!view.SeparateImageResource || view.ImageResource.ResourceID != serverImageResourceID {
		t.Fatalf("incorrect image inventory: %+v", view)
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/config", nil))
	for _, text := range []string{serverImageResourceID, images.snapshot.Endpoint, `id="foundry-image-resource-id"`, `id="foundry-image-endpoint"`} {
		if !strings.Contains(page.Body.String(), text) {
			t.Errorf("settings omit %q", text)
		}
	}
	for _, resourceID := range []string{serverResourceID, serverImageResourceID} {
		cached, known, err := s.store.LoadCatalog(t.Context(), resourceID)
		if err != nil || !known || cached.ResourceID != resourceID {
			t.Fatalf("resource cache missing: %s, %v", resourceID, err)
		}
	}
}

func TestSeparateImageRefreshFailuresRemainIndependent(t *testing.T) {
	s, _, primary, images, _, _ := separateImageTestServer(t)
	if err := s.refreshFoundry(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.runChecks(t.Context(), true)
	if !s.ready.uploadsAllowed() {
		t.Fatal("initial core checks failed")
	}
	primary.fail = true
	before := images.refreshes
	if err := s.refreshFoundry(t.Context()); err == nil || images.refreshes != before+1 {
		t.Fatal("a primary discovery failure prevented refreshing the image resource")
	}
	s.runChecks(t.Context(), false)
	primary.fail, images.fail = false, true
	if err := s.refreshFoundry(t.Context()); err == nil {
		t.Fatal("image discovery failure was hidden")
	}
	last, _ := s.ready.lastResults()
	results := s.currentInventoryResults(last)
	for _, result := range results {
		switch result.Key {
		case "foundry-inventory":
			if !result.OK {
				t.Fatalf("successful refresh kept a stale ARM error: %+v", result)
			}
		case "foundry-image-inventory":
			if result.OK || !strings.Contains(result.Detail, "permission denied") {
				t.Fatalf("image failure was not isolated in its own check: %+v", result)
			}
		case "foundry-image-catalog":
			t.Fatal("split resource still exposes the unrelated Models API catalog")
		}
	}
	if !s.ready.uploadsAllowed() || len(s.cfg.ImageFoundryStatus().Catalog.Deployments) != 1 {
		t.Fatal("image discovery failure disabled uploads or erased the prior image catalog")
	}
	images.fail = false
	images.snapshot.Deployments[0].ModelVersion = "new-version"
	if err := s.refreshFoundry(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.ready.uploadsAllowed() || !s.ready.verified() {
		t.Fatal("image-only metadata changes invalidated core readiness")
	}
}

func TestSeparateImageCatalogsLoadIndependently(t *testing.T) {
	s, _, primary, images, primaryCalls, imageCalls := separateImageTestServer(t)
	if err := s.refreshFoundry(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, imageID := range []string{serverImageResourceID, serverImageResourceID + "-other"} {
		t.Run(imageID, func(t *testing.T) {
			cfg := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{}, config.Overrides{})
			cfg.ConfigureFoundry(serverResourceID, primary, nil)
			cfg.ConfigureImageFoundry(imageID, images, nil)
			current := cfg.Get()
			current.ChatDeployment, current.ImageDeployment = "chat-prod", "gpt-image-2"
			if err := cfg.Save(current); err != nil {
				t.Fatal(err)
			}
			restarted := New(cfg, s.store, s.logs)
			t.Cleanup(restarted.Close)
			if cfg.Get().Endpoint != primary.snapshot.Endpoint {
				t.Fatal("image cache changed the primary endpoint")
			}
			if imageID == serverImageResourceID {
				if cfg.Get().ImageHost() != images.snapshot.Endpoint {
					t.Fatal("image resource cache was not loaded at startup")
				}
			} else {
				if cfg.Get().ImageHost() != "" {
					t.Fatal("an uncached image resource fell back to a different resource")
				}
				if err := restarted.llm.VerifyImage(t.Context(), "gpt-image-2"); err == nil {
					t.Fatal("an undiscovered image resource was probed")
				}
			}
		})
	}
	if primaryCalls.Load() != 0 || imageCalls.Load() != 0 {
		t.Fatal("loading catalogs issued inference requests")
	}
}

func TestImageCheckInvalidationKeepsCoreReadiness(t *testing.T) {
	ready := &readiness{}
	epoch := ready.epoch()
	ready.finish(epoch, true, true, true, false, []checkResult{{Key: "image-api", OK: true}}, true)
	ready.invalidateImageChecks("Image metadata changed")
	if !ready.uploadsAllowed() || !ready.verified() {
		t.Fatal("image invalidation reset core readiness")
	}
	if ready.finish(epoch, true, true, true, false, nil, true) {
		t.Fatal("an in-flight check restored outdated image state")
	}
	results, _ := ready.lastResults()
	if len(results) != 1 || !results[0].Skipped || results[0].OK {
		t.Fatalf("image result was not marked stale: %+v", results)
	}
}
