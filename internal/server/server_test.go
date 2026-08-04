package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// newTestServer wires a server against a throwaway database.
func newTestServer(t *testing.T, language string) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()

	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfgStore := config.NewStore(filepath.Join(dir, "config.json"), config.Keys{}, config.Overrides{})
	if _, err := cfgStore.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := cfgStore.Get()
	cfg.Language = language
	if err := cfgStore.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	srv := New(cfgStore, store)
	return srv, srv.Routes()
}

// TestRoutesRenderInEveryLanguage is a smoke test: template errors only surface
// at render time, so every page is executed once per supported language.
func TestRoutesRenderInEveryLanguage(t *testing.T) {
	for _, opt := range i18n.Options() {
		t.Run(opt.Code, func(t *testing.T) {
			srv, handler := newTestServer(t, opt.Code)

			chatID, err := srv.store.CreateChat(t.Context(), untitled)
			if err != nil {
				t.Fatalf("create chat: %v", err)
			}
			if _, err := srv.store.AddMessage(t.Context(), chatID, "user", "hello **world**"); err != nil {
				t.Fatalf("add message: %v", err)
			}

			paths := []string{
				"/healthz",
				"/chat/1",
				"/config",
				"/stats",
				"/status",
				"/static/app.css",
			}
			for _, path := range paths {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d, want 200", path, rec.Code)
				}
				if strings.Contains(rec.Body.String(), "{{") {
					t.Errorf("GET %s rendered an unresolved template action", path)
				}
			}
		})
	}
}

// TestSecurityHeaders verifies the defensive headers are present on responses.
func TestSecurityHeaders(t *testing.T) {
	_, handler := newTestServer(t, i18n.EN)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP is missing frame-ancestors: %q", csp)
	}
}

// TestStaticAssetsRevalidate makes sure the embedded assets answer a repeated
// request with 304 instead of resending the payload.
func TestStaticAssetsRevalidate(t *testing.T) {
	_, handler := newTestServer(t, i18n.EN)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static asset served without an ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Errorf("repeated request = %d, want 304", second.Code)
	}
}

// TestInvalidSearchEndpointRejected covers the SSRF guard on the settings form.
func TestInvalidSearchEndpointRejected(t *testing.T) {
	srv, handler := newTestServer(t, i18n.EN)

	form := strings.NewReader("search_provider=searxng&search_endpoint=file:///etc/passwd")
	req := httptest.NewRequest(http.MethodPost, "/config", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := srv.cfg.Get().SearchEndpoint; got != "" {
		t.Errorf("invalid endpoint was stored: %q", got)
	}
}
