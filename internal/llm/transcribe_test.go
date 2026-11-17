package llm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
)

func TestTranscribeRejectsPartialDocument(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			http.Error(w, "unreadable page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"First page\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{API: "test"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint, cfg.ChatDeployment = backend.URL+"/openai/v1", "gpt-4o"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	client := New(store)
	result, err := client.Transcribe(t.Context(), "Transcribe", func(page, total int) string { return fmt.Sprintf("Page %d of %d", page, total) },
		[]ImageContent{{MIME: "image/png", Data: []byte("one")}, {MIME: "image/png", Data: []byte("two")}})
	if err == nil || result.Text != "" || !strings.Contains(err.Error(), "page 2 of 2") || calls.Load() != 2 {
		t.Fatalf("partial success: result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}
