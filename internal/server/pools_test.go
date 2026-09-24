package server

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestPooledDurableImageTurnsAlwaysEditLatestSuccessfulImage(t *testing.T) {
	var sequence atomic.Int64
	type observed struct{ provider, source, output string }
	requests := make(chan observed, 4)
	backend := func(provider string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			call := observed{provider: provider, output: fmt.Sprintf("%s-%d", provider, sequence.Add(1))}
			if strings.HasSuffix(r.URL.Path, "/edits") {
				file, _, err := r.FormFile("image")
				if err != nil {
					t.Error(err)
					return
				}
				raw, err := io.ReadAll(file)
				_ = file.Close() // Fixture bytes are consumed.
				if err != nil {
					t.Error(err)
				}
				call.source = string(raw)
			} else if !strings.HasSuffix(r.URL.Path, "/generations") {
				t.Errorf("unexpected operation: %s", r.URL.Path)
			}
			requests <- call
			if _, err := fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString([]byte(call.output))); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(server.Close)
		return server
	}
	a, b := backend("A"), backend("B")
	s, handler, source, _ := newFoundryTestServer(t, "en")
	root := source.snapshot
	root.Endpoint = a.URL + "/openai/v1"
	replica := root
	replica.Endpoint = b.URL + "/openai/v1"
	replica.ResourceID += "-poland"
	root.Accounts = []foundry.Snapshot{replica}
	if err := s.cfg.SetCatalog(root); err != nil {
		t.Fatal(err)
	}
	chat, _ := root.Find("chat-prod")
	image, _ := root.Find("pictures-prod")
	cfg := s.cfg.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{
		foundry.Chat:       {root.Key(chat)},
		foundry.Images:     {root.Key(image), replica.Key(image)},
		foundry.ImageEdits: {root.Key(image), replica.Key(image)},
	}
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	id, err := s.store.CreateChat(t.Context(), "Image refinement", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	last := ""
	for i, provider := range []string{"A", "A", "B", "A"} {
		edit := "1"
		if i == 0 {
			edit = "0"
		}
		stream := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Refine&mode=image&edit="+edit))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, stream, nil))
		s.jobs.Wait()
		var request observed
		select {
		case request = <-requests:
		default:
			t.Fatal("durable image turn never reached its provider")
		}
		if request.provider != provider || request.source != last {
			t.Fatalf("step %d lost the latest image: %+v; previous=%q", i, request, last)
		}
		stored, err := s.store.LatestImage(t.Context(), id)
		if err != nil || string(stored.Data) != request.output {
			t.Fatalf("successful image not persisted: %v", err)
		}
		last = request.output
	}
}

func TestPoolCheckboxPersistenceAndInvalidEmbeddingAtomicity(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			s, handler, source, calls := newFoundryTestServer(t, language)
			root := source.snapshot
			b := root
			b.ResourceID += "-poland"
			b.Location = "polandcentral"
			b.Deployments = slices.Clone(root.Deployments)
			root.Accounts = []foundry.Snapshot{b}
			source.snapshot = root
			refreshed := postFoundryForm(handler, "/config/deployments/refresh", nil)
			if !strings.Contains(refreshed.Body.String(), "polandcentral") || calls.Load() != 0 {
				t.Fatal("replica not displayed, or metadata refresh invoked inference")
			}
			d, _ := root.Find("chat-prod")
			v, _ := root.Find("vectors-prod")
			form := url.Values{
				"language": {language}, "deployment_pools": {"1"},
				"enabled_chat":       {root.Key(d), b.Key(d)},
				"enabled_embeddings": {root.Key(v), b.Key(v)},
			}
			response := postFoundryForm(handler, "/config", form)
			if response.Code != http.StatusOK || len(s.cfg.Get().EnabledDeployments[foundry.Chat]) != 2 {
				t.Fatalf("checkboxes did not persist: %d %s", response.Code, response.Body.String())
			}
			if s.cfg.ImagesConfigured() {
				t.Fatal("unchecked optional pool was silently enabled")
			}
			for range 2 {
				id, err := s.newChat(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				chat, err := s.store.GetChat(t.Context(), id)
				if err != nil || chat.Model != "" {
					t.Fatal("new pool chat was silently pinned")
				}
			}
			for i := range b.Deployments {
				if b.Deployments[i].Name == "vectors-prod" {
					b.Deployments[i].ModelVersion = "different"
				}
			}
			root.Accounts = []foundry.Snapshot{b}
			if err := s.cfg.SetCatalog(root); err != nil {
				t.Fatal(err)
			}
			response = postFoundryForm(handler, "/config", form)
			if !strings.Contains(response.Body.String(), s.t("foundry.embedding_pool_invalid")) {
				t.Fatal("incompatible embedding pool did not report its localized error")
			}
			if _, known, err := s.store.ActiveEmbeddingProfile(t.Context()); err != nil || known {
				t.Fatal("saving selections relabeled the index")
			}
		})
	}
}
