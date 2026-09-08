package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestFoundryAPICatalogRestoresImageSelection(t *testing.T) {
	srv, handler, source, calls := newFoundryTestServer(t, "en")
	source.images = []foundry.Deployment{{
		Name: "gpt-image-2", ModelName: "gpt-image-2", ModelFormat: "OpenAI", Source: foundry.ModelsAPISource,
	}}
	response := postFoundryForm(handler, "/config/deployments/refresh", nil)
	model, exists := srv.cfg.FoundryStatus().Catalog.Find("gpt-image-2")
	if response.Code != http.StatusOK || !exists || model.Source != foundry.ModelsAPISource || calls.Load() != 0 {
		t.Fatal("API-listed image model was not added by metadata-only discovery")
	}
	if !strings.Contains(response.Body.String(), `<option value="gpt-image-2"`) ||
		!strings.Contains(response.Body.String(), "Models API") {
		t.Fatal("API image choice or its origin was not shown")
	}
	postFoundryForm(handler, "/config", url.Values{
		"language": {"en"}, "chat_deployment": {"chat-prod"}, "image_deployment": {"gpt-image-2"},
	})
	if srv.cfg.Get().ImageDeployment != "gpt-image-2" || !srv.cfg.ImagesConfigured() {
		t.Fatal("API image selection did not persist")
	}
	results := srv.runChecks(t.Context(), true)
	found := false
	for _, result := range results {
		if result.Target == "gpt-image-2" {
			found = true
			if !result.OK || !result.Info || result.Skipped {
				t.Fatalf("image probe must be informative rather than prove generation: %+v", result)
			}
		}
	}
	if !found {
		t.Fatal("selected API image model was not checked")
	}
}

func TestFoundryImageCatalogFailurePreservesScopedChoices(t *testing.T) {
	srv, handler, source, _ := newFoundryTestServer(t, "en")
	source.images = []foundry.Deployment{
		{Name: "gpt-image-2", ModelName: "gpt-image-2", ModelFormat: "OpenAI", Source: foundry.ModelsAPISource},
		{Name: "pictures-prod", ModelName: "gpt-image-2", ModelFormat: "OpenAI", Source: foundry.ModelsAPISource},
	}
	postFoundryForm(handler, "/config/deployments/refresh", nil)
	catalog := srv.cfg.FoundryStatus().Catalog
	if model, _ := catalog.Find("pictures-prod"); model.Source != "" || model.ModelName != "gpt-image-1" {
		t.Fatal("API catalog overrode an ARM deployment alias")
	}
	source.imageErr = errors.New("image catalog permission denied")
	source.images = nil
	response := postFoundryForm(handler, "/config/deployments/refresh", nil)
	catalog = srv.cfg.FoundryStatus().Catalog
	if _, exists := catalog.Find("gpt-image-2"); !exists || catalog.ImageCatalogError == "" {
		t.Fatal("partial refresh erased cached image choices or hid its failure")
	}
	if !strings.Contains(response.Body.String(), "image catalog permission denied") {
		t.Fatal("partial refresh failure was not displayed")
	}
	stored, exists, err := srv.store.LoadCatalog(t.Context(), serverResourceID)
	if err != nil || !exists || stored.ImageCatalogError == "" {
		t.Fatalf("partial catalog state was not persisted: %v", err)
	}
	if _, exists := stored.Find("gpt-image-2"); !exists {
		t.Fatal("cached image choices would disappear on restart")
	}
	source.snapshot.Endpoint = "https://different.example/openai/v1"
	postFoundryForm(handler, "/config/deployments/refresh", nil)
	if _, exists := srv.cfg.FoundryStatus().Catalog.Find("gpt-image-2"); exists {
		t.Fatal("old API models were carried across an endpoint change")
	}
}

func TestFoundrySettingsUseReadableGroupsAndReadonlyFields(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			srv, handler, _, _ := newFoundryTestServer(t, language)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/config", nil))
			body := response.Body.String()
			for _, expected := range []string{
				`id="foundry-resource-id"`, `id="foundry-endpoint"`, `class="config-readonly"`,
				`class="foundry-model-list"`, `class="foundry-capabilities"`,
				srv.t("config.verify_pending"), srv.t("config.verify_saved_hint"),
			} {
				if !strings.Contains(body, expected) {
					t.Errorf("settings omit %q", expected)
				}
			}
			if strings.Contains(body, `class="foundry-table"`) || strings.Contains(body, `<code>`+serverResourceID) {
				t.Fatal("old compressed table or code-styled identifiers remain")
			}
		})
	}
}

func TestFoundryUnconfiguredChecksAreSkipped(t *testing.T) {
	srv, _, _, calls := newFoundryTestServer(t, "en")
	results := srv.runChecks(t.Context(), true)
	for _, result := range results {
		switch result.Name {
		case srv.t("check.chat_endpoint"), srv.t("check.embedding_endpoint"),
			srv.t("check.vision_endpoint"), srv.t("check.image_endpoint"):
			if !result.Skipped || result.State() != "skipped" {
				t.Errorf("unconfigured feature shown as a failure: %+v", result)
			}
		}
	}
	if calls.Load() != 0 || srv.ready.uploadsAllowed() || srv.ready.verified() {
		t.Fatal("unconfigured checks called inference or reported readiness")
	}
	cached, checkedAt := srv.ready.lastResults()
	if len(cached) == 0 || checkedAt.IsZero() {
		t.Fatal("current check results were not cached for the settings dialog")
	}
}

func TestFoundryChecksShowSavedSettingsAndLatestScope(t *testing.T) {
	srv, handler, _, _ := newFoundryTestServer(t, "en")
	cfg := srv.cfg.Get()
	cfg.ChatDeployment, cfg.ImageDeployment = "chat-prod", "pictures-prod"
	if err := srv.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	response := postFoundryForm(handler, "/verify", url.Values{
		"chat_deployment": {"unsaved-chat"}, "image_deployment": {"unsaved-image"},
	})
	if strings.Contains(response.Body.String(), "unsaved-chat") || strings.Contains(response.Body.String(), "unsaved-image") {
		t.Fatal("connection checks used unsaved form values")
	}
	srv.runChecks(t.Context(), false)
	results, _ := srv.ready.lastResults()
	for _, result := range results {
		if result.Name == srv.t("check.image_endpoint") {
			if result.Target != "pictures-prod" || !result.Skipped {
				t.Fatalf("periodic results falsely reused an earlier image probe: %+v", result)
			}
			return
		}
	}
	t.Fatal("latest check scope was not visible in cached results")
}
