package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAssetURLsFollowContent(t *testing.T) {
	oldAssets, err := newStaticHandler(fstest.MapFS{"app.css": &fstest.MapFile{Data: []byte("old")}})
	if err != nil {
		t.Fatal(err)
	}
	newAssets, err := newStaticHandler(fstest.MapFS{"app.css": &fstest.MapFile{Data: []byte("new")}})
	if err != nil {
		t.Fatal(err)
	}
	oldURL, err := oldAssets.URL("app.css")
	if err != nil {
		t.Fatal(err)
	}
	newURL, err := newAssets.URL("app.css")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(newURL)
	if err != nil || parsed.Query().Get("v") == "" || oldURL == newURL {
		t.Fatalf("asset URL is not content-versioned: %s, %s", oldURL, newURL)
	}
	first := httptest.NewRecorder()
	oldAssets.ServeHTTP(first, httptest.NewRequest(http.MethodGet, oldURL, nil))
	request := httptest.NewRequest(http.MethodGet, newURL, nil)
	request.Header.Set("If-None-Match", first.Header().Get("ETag"))
	updated := httptest.NewRecorder()
	newAssets.ServeHTTP(updated, request)
	if updated.Code != http.StatusOK || updated.Body.String() != "new" {
		t.Fatal("changed assets must not reuse the old response")
	}
	if _, err := newAssets.URL("missing.js"); err == nil {
		t.Fatal("unknown template assets must produce an error")
	}
}

func TestPagesAndSettingsUseCurrentStylesheet(t *testing.T) {
	server, handler := newTestServer(t, "en")
	stylesheet, err := server.assets.URL("app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/logs", "/stats", "/config"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code == http.StatusSeeOther {
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/chat/1", nil))
		}
		if !strings.Contains(response.Body.String(), stylesheet) {
			t.Errorf("%s did not use the content-versioned stylesheet", path)
		}
		if path == "/config" && !strings.Contains(response.Body.String(),
			`hx-swap-oob="outerHTML:head link[rel='stylesheet'][href^='/static/app.css']"`) {
			t.Fatal("settings must refresh the stylesheet even on an old open page")
		}
	}
}
