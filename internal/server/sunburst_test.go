package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/llm"
)

func TestSunburstRetryUsesCurrentIndependentOptions(t *testing.T) {
	requests := make(chan struct{ Size, Quality string }, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Size, Quality string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		_, _ = fmt.Fprint(w, `{"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aVr0AAAAASUVORK5CYII="}]}`)
	}))
	defer backend.Close()
	s, handler, id := fluxServer(t, backend.URL, "en")
	catalog := s.cfg.FoundryStatus().Catalog
	d := foundry.Deployment{Name: "sun", ModelName: llm.SunburstModel, ModelFormat: "OpenAI", ModelVersion: "2026-09-08", ProvisioningState: "Succeeded"}
	catalog.Deployments = append(catalog.Deployments, d)
	if err := s.cfg.SetCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg.Get()
	cfg.EnabledDeployments[foundry.Images] = []string{catalog.Key(d)}
	cfg.EnabledDeployments[foundry.ImageEdits] = []string{catalog.Key(d)}
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	complete := func(response *httptest.ResponseRecorder) {
		t.Helper()
		stream := submittedGenerationURL(t, response)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, stream, nil))
		s.jobs.Wait()
	}
	complete(sendGeneration(t, handler, id, "message=Draw+a+tree&mode=image&image_model=gpt-image-2.5-sunburst&sunburst_size=1536x864&sunburst_quality=xhigh&image_quality=low"))
	first := <-requests
	if first.Size != "1536x864" || first.Quality != "xhigh" {
		t.Fatalf("wrong initial options: %+v", first)
	}
	turns, err := s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 1 {
		t.Fatalf("missing turn %v", err)
	}
	complete(retryRequest(t, handler, id, turns[0].ID, "mode=image&edit=0&image_model=gpt-image-2.5-sunburst&sunburst_size=2560x1440&sunburst_quality=max"))
	second := <-requests
	if second.Size != "2560x1440" || second.Quality != "max" {
		t.Fatalf("retry lost current Sunburst parameters: %+v", second)
	}
	turns, err = s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 2 {
		t.Fatalf("missing retry %v", err)
	}
	for _, turn := range turns {
		var options generationOptions
		if err := json.Unmarshal([]byte(turn.Request), &options); err != nil {
			t.Fatal(err)
		}
		if options.ImageOptions.Model != llm.SunburstModel || options.ImageOptions.Size != options.ImageOptions.SunburstSize || options.ImageOptions.Quality != options.ImageOptions.SunburstQuality {
			t.Fatal("stored turn lost model-specific options")
		}
	}
}
