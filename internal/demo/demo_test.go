package demo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// TestConversationsParse makes sure the demo content of every supported
// language stays readable for the seeder.
func TestConversationsParse(t *testing.T) {
	for _, lang := range i18n.Options() {
		convs, err := conversations(lang.Code)
		if err != nil {
			t.Fatalf("conversations(%q): %v", lang.Code, err)
		}
		if len(convs) == 0 {
			t.Fatalf("no demo conversations for %q", lang.Code)
		}
		for _, conv := range convs {
			if len(conv.Messages) < 2 {
				t.Errorf("%s/%s: expected at least a question and an answer", lang.Code, conv.Key)
			}
			if conv.Messages[0].Role != "user" {
				t.Errorf("%s/%s: conversation must start with a user message", lang.Code, conv.Key)
			}
			if conv.Image != "" && !strings.Contains(conv.Messages[len(conv.Messages)-1].Content, imagePlaceholder) {
				t.Errorf("%s/%s: image conversation without %s placeholder", lang.Code, conv.Key, imagePlaceholder)
			}
		}
		if reply(lang.Code) == "" {
			t.Errorf("no stub reply for %q", lang.Code)
		}
	}
}

// TestConversationKeysMatch keeps the sections of both languages aligned, since
// the screenshot tooling addresses them by the same key.
func TestConversationKeysMatch(t *testing.T) {
	var reference []string
	for _, lang := range i18n.Options() {
		convs, err := conversations(lang.Code)
		if err != nil {
			t.Fatalf("conversations(%q): %v", lang.Code, err)
		}
		keys := make([]string, 0, len(convs))
		for _, conv := range convs {
			keys = append(keys, conv.Key)
		}
		if reference == nil {
			reference = keys
			continue
		}
		if strings.Join(keys, ",") != strings.Join(reference, ",") {
			t.Errorf("language %q has sections %v, expected %v", lang.Code, keys, reference)
		}
	}
}

// TestSeed checks that a fresh database ends up with the complete demo content.
func TestSeed(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "demo.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	idx, err := Seed(ctx, store, dbPath, "en")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(idx.Chats) == 0 {
		t.Fatal("index holds no chats")
	}

	imageChat, ok := idx.Chats["image"]
	if !ok {
		t.Fatal("no image section seeded")
	}
	msgs, err := store.ListMessages(ctx, imageChat)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	last := msgs[len(msgs)-1].Content
	if strings.Contains(last, imagePlaceholder) || !strings.Contains(last, "](/images/") {
		t.Errorf("image placeholder was not replaced: %q", last)
	}

	docChat := idx.Chats["documents"]
	chunks, err := store.CountChunksByChat(ctx, docChat)
	if err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks == 0 {
		t.Error("documents section has no embedded chunks")
	}

	usage, err := store.UsageByDay(ctx, 30)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(usage) != usageDays {
		t.Errorf("got %d days of statistics, want %d", len(usage), usageDays)
	}

	// Seeding twice must not duplicate anything.
	if _, err := Seed(ctx, store, dbPath, "en"); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	chats, err := store.ListChats(ctx)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != len(idx.Chats) {
		t.Errorf("got %d chats after seeding twice, want %d", len(chats), len(idx.Chats))
	}
}

// TestBackendChatStream verifies that the stub answers the way the client
// expects: as a token stream that ends with usage and [DONE].
func TestBackendChatStream(t *testing.T) {
	backend, err := StartBackend("en")
	if err != nil {
		t.Fatalf("start backend: %v", err)
	}
	backend.streamDelay = 0
	defer func() { _ = backend.Close() }()

	body := strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(backend.URL()+"/openai/deployments/model-router/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stream := string(raw)
	if !strings.Contains(stream, "data: [DONE]") {
		t.Error("stream does not end with [DONE]")
	}
	if !strings.Contains(stream, `"usage"`) {
		t.Error("stream reports no token usage")
	}
	if !strings.Contains(stream, `"delta"`) {
		t.Error("stream contains no deltas")
	}
}

// TestBackendEmbeddings checks the vector length and the response shape.
func TestBackendEmbeddings(t *testing.T) {
	backend, err := StartBackend("en")
	if err != nil {
		t.Fatalf("start backend: %v", err)
	}
	defer func() { _ = backend.Close() }()

	resp, err := http.Post(backend.URL()+"/openai/deployments/text-embedding-3-large/embeddings",
		"application/json", strings.NewReader(`{"input":["alpha","beta"]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("got %d vectors, want 2", len(out.Data))
	}
	if len(out.Data[0].Embedding) != embeddingDim {
		t.Errorf("vector length %d, want %d", len(out.Data[0].Embedding), embeddingDim)
	}
}
