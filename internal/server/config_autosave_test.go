package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestConfigAutosaveSuccessAndRejection(t *testing.T) {
	s, handler, _, _ := newFoundryTestServer(t, "en")
	target := s.cfg.Targets(foundry.Chat)[0].Key
	save := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Config-Autosave", "1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	form := url.Values{"language": {"en"}, "deployment_pools": {"1"},
		"enabled_chat": {target}, "system_prompt": {"Saved first"}}
	rec := save(form)
	var result struct {
		Saved, Reload        bool
		Error, Checks, Index string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK ||
		!result.Saved || result.Reload || !strings.Contains(result.Checks, "check-list") || !strings.Contains(result.Index, "embedding-index") {
		t.Fatalf("autosave response=%d %s err=%v", rec.Code, rec.Body.String(), err)
	}
	if s.cfg.Get().SystemPrompt != "Saved first" {
		t.Fatal("success returned without persistence")
	}
	form.Set("enabled_chat", "missing-deployment")
	form.Set("system_prompt", "Rejected edit")
	rec = save(form)
	result.Saved = true
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusBadRequest || result.Saved || result.Error == "" {
		t.Fatalf("rejection was not explicit: %d %s", rec.Code, rec.Body.String())
	}
	if s.cfg.Get().SystemPrompt != "Saved first" {
		t.Fatal("rejected autosave modified settings")
	}
	form.Set("enabled_chat", target)
	form.Set("language", "de")
	rec = save(form)
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.Saved || !result.Reload || rec.Header().Get("HX-Refresh") != "" {
		t.Fatalf("language autosave must defer reload to the client: %s", rec.Body.String())
	}
}

func TestConfigAutosaveWriteFailureIsNotSuccess(t *testing.T) {
	s, handler := newTestServer(t, "en")
	// A directory cannot be replaced by the configuration file's atomic rename.
	s.cfg = config.NewStore(t.TempDir(), config.Keys{}, config.Overrides{})
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader("language=en&system_prompt=must-not-persist"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Config-Autosave", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), `"saved":true`) ||
		s.cfg.Get().SystemPrompt == "must-not-persist" {
		t.Fatalf("write failure reported success: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigCombinesCardsAndKeepsOperationCheckboxes(t *testing.T) {
	s, handler, _, _ := newFoundryTestServer(t, "en")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	body := rec.Body.String()
	if strings.Contains(body, `type="submit"`) || strings.Contains(body, s.t("foundry.selection_details")) ||
		strings.Contains(body, s.t("config.check_details")) {
		t.Fatal("removed controls or explanation remain")
	}

	if strings.Index(body, `name="language"`) > strings.Index(body, `id="verify-results"`) {
		t.Fatal("language not first")
	}
	for _, name := range []string{"enabled_chat", "enabled_embeddings", "enabled_images", "enabled_image_edits", "enabled_vision"} {
		if !strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("missing independent toggle %s", name)
		}
	}
	if strings.Contains(body, `class="deployment-pool"`) {
		t.Fatal("duplicate pool list remains")
	}
	if strings.Count(body, `<strong>chat-prod</strong>`) != 1 {
		t.Fatal("chat deployment shown more than once")
	}
}
