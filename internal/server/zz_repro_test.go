package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/logbuf"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestReproAttachments(t *testing.T) {
	var mu sync.Mutex
	var lastChat map[string]any

	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "embeddings") {
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			out := map[string]any{"usage": map[string]int{"total_tokens": 1}}
			data := []map[string]any{}
			for i, in := range req.Input {
				// crude deterministic embedding
				var v [4]float64
				for j, c := range in {
					v[j%4] += float64(c) / 1000
				}
				data = append(data, map[string]any{"index": i, "embedding": v})
			}
			out["data"] = data
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		if _, ok := req["stream"]; ok { if b, _ := req["stream"].(bool); b && lastChat == nil { lastChat = req } }
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer azure.Close()

	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfgStore := config.NewStore(filepath.Join(dir, "config.json"), config.Keys{API: "k"}, config.Overrides{})
	if _, err := cfgStore.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := cfgStore.Get()
	cfg.Endpoint = azure.URL
	cfg.ChatDeployment = "router"
	cfg.EmbeddingDeployment = "embed"
	if err := cfgStore.Save(cfg); err != nil {
		t.Fatal(err)
	}
	srv := New(cfgStore, store, logbuf.New(50))
	handler := srv.Routes()
	srv.runChecks(t.Context(), false)
	t.Logf("uploads allowed: %v", srv.ready.uploadsAllowed())

	chatID, err := store.CreateChat(t.Context(), untitled, "", "")
	if err != nil {
		t.Fatal(err)
	}

	upload := func(name, content string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/chat/1/documents", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		t.Logf("upload %s -> %d body=%s", name, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	upload("alpha.txt", "The alpha document says the sky is green.")
	upload("beta.txt", "The beta document says the sea is purple.")

	docs, _ := store.ListDocumentsByChat(t.Context(), chatID)
	t.Logf("docs=%d", len(docs))
	for _, d := range docs {
		t.Logf("doc %d %s chunks=%d", d.ID, d.Name, d.Chunks)
	}

	// send message
	form := strings.NewReader("message=" + "Was+steht+in+den+Anhaengen")
	req := httptest.NewRequest(http.MethodPost, "/chat/1/send", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	t.Logf("send -> %d", rec.Code)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/chat/1/generate", nil))
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.MarshalIndent(lastChat, "", " ")
	t.Logf("chat request:\n%s", b)
}
