package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/logbuf"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// attachmentFixture wires a server against a stub endpoint that answers both the
// embedding and the chat calls, and records the first streamed chat request so a
// test can inspect what the model actually received.
type attachmentFixture struct {
	srv     *Server
	handler http.Handler

	mu         sync.Mutex
	chatPrompt string
}

func newAttachmentFixture(t *testing.T) *attachmentFixture {
	t.Helper()
	fx := &attachmentFixture{}

	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			data := make([]map[string]any, 0, len(req.Input))
			for i, in := range req.Input {
				// A cheap deterministic embedding: enough to make the cosine
				// search reproducible without a real endpoint.
				vec := make([]float64, 4)
				for j, c := range in {
					vec[j%4] += float64(c) / 1000
				}
				data = append(data, map[string]any{"index": i, "embedding": vec})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":  data,
				"usage": map[string]int{"total_tokens": 1},
			})
			return
		}

		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream && len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			fx.mu.Lock()
			if fx.chatPrompt == "" {
				fx.chatPrompt = req.Messages[0].Content
			}
			fx.mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(azure.Close)

	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfgStore := config.NewStore(filepath.Join(dir, "config.json"), config.Keys{API: "key"}, config.Overrides{})
	if _, err := cfgStore.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := cfgStore.Get()
	cfg.Endpoint = azure.URL
	cfg.ChatDeployment = "model-router"
	cfg.EmbeddingDeployment = "text-embedding-3-large"
	cfg.ImageDeployment = "gpt-image-1"
	if err := cfgStore.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fx.srv = New(cfgStore, store, logbuf.New(50))
	fx.handler = fx.srv.Routes()
	// Uploads are gated behind a verified connection.
	fx.srv.runChecks(t.Context(), false)
	if !fx.srv.ready.uploadsAllowed() {
		t.Fatal("uploads must be allowed after a successful check")
	}
	return fx
}

// upload posts a single file to the document endpoint of a chat.
func (fx *attachmentFixture) upload(t *testing.T, chatID int64, name, content string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/chat/"+strconv.FormatInt(chatID, 10)+"/documents", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	fx.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s = %d, want 200", name, rec.Code)
	}
	return rec.Body.String()
}

// TestAttachmentsReachTheModelContext covers the whole chain for several
// attachments: upload -> ingestion -> retrieval -> system prompt. Every attached
// document must be named in the prompt and contribute a context section, so a
// second attachment cannot be crowded out by the first one.
func TestAttachmentsReachTheModelContext(t *testing.T) {
	fx := newAttachmentFixture(t)

	chatID, err := fx.srv.store.CreateChat(t.Context(), untitled, "", "")
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	fx.upload(t, chatID, "alpha.txt", "The alpha document says the sky is green.")
	fx.upload(t, chatID, "beta.txt", "The beta document says the sea is purple.")

	docs, err := fx.srv.store.ListDocumentsByChat(t.Context(), chatID)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("stored %d documents, want 2", len(docs))
	}
	for _, d := range docs {
		if d.Chunks == 0 {
			t.Errorf("document %q was stored without chunks", d.Name)
		}
	}

	form := strings.NewReader("message=what+do+the+attachments+say")
	req := httptest.NewRequest(http.MethodPost, "/chat/"+strconv.FormatInt(chatID, 10)+"/send", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	fx.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d, want 200", rec.Code)
	}

	gen := httptest.NewRecorder()
	fx.handler.ServeHTTP(gen, httptest.NewRequest(http.MethodGet, "/chat/"+strconv.FormatInt(chatID, 10)+"/generate", nil))

	fx.mu.Lock()
	prompt := fx.chatPrompt
	fx.mu.Unlock()

	for _, want := range []string{
		"Attached documents:",
		"alpha.txt",
		"beta.txt",
		"the sky is green",
		"the sea is purple",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// TestImageAttachmentRejectedInChatMode guards the routing of image uploads: in
// chat mode an image cannot be turned into an edit source silently, because the
// chat context is built from ingested documents only.
func TestImageAttachmentRejectedInChatMode(t *testing.T) {
	fx := newAttachmentFixture(t)

	chatID, err := fx.srv.store.CreateChat(t.Context(), untitled, "", "")
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	body := fx.upload(t, chatID, "photo.png", "\x89PNG\r\n\x1a\n")

	imgs, err := fx.srv.store.ListImagesByKind(t.Context(), chatID, storage.ImageUpload)
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(imgs) != 0 {
		t.Errorf("an image in chat mode must not be stored as an edit source, got %d", len(imgs))
	}
	if !strings.Contains(body, "photo.png") {
		t.Errorf("the response must name the rejected file:\n%s", body)
	}

	// In image mode the same upload becomes the source for editing.
	if err := fx.srv.store.UpdateChatMode(t.Context(), chatID, storage.ChatModeImage); err != nil {
		t.Fatalf("update mode: %v", err)
	}
	fx.upload(t, chatID, "photo.png", "\x89PNG\r\n\x1a\n")
	imgs, err = fx.srv.store.ListImagesByKind(t.Context(), chatID, storage.ImageUpload)
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(imgs) != 1 {
		t.Errorf("image mode must keep the upload as an edit source, got %d", len(imgs))
	}
}
