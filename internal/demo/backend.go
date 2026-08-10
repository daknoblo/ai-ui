package demo

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxRequestBytes caps the request bodies the stub reads. Image edits send the
// source image as multipart, everything else is small JSON.
const maxRequestBytes = 32 << 20

// Backend is a stand-in for the Azure-compatible endpoints. It answers chat,
// embedding and image requests with generated demo data so the interface can be
// explored and captured without any cloud resources. It listens on loopback
// only and deliberately performs no authentication: it never returns anything
// but demo content.
type Backend struct {
	lang        string
	streamDelay time.Duration
	listener    net.Listener
	server      *http.Server
}

// StartBackend starts the stub on a free loopback port.
func StartBackend(lang string) (*Backend, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Backend{lang: lang, streamDelay: 25 * time.Millisecond, listener: ln}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /openai/deployments/{deployment}/chat/completions", b.handleChat)
	mux.HandleFunc("POST /openai/deployments/{deployment}/embeddings", b.handleEmbeddings)
	mux.HandleFunc("POST /openai/deployments/{deployment}/images/generations", b.handleImage)
	mux.HandleFunc("POST /openai/deployments/{deployment}/images/edits", b.handleImageEdit)

	b.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := b.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("demo backend", "err", err)
		}
	}()
	return b, nil
}

// URL returns the base URL the application talks to.
func (b *Backend) URL() string {
	return "http://" + b.listener.Addr().String()
}

// Close shuts the stub down.
func (b *Backend) Close() error {
	return b.server.Close()
}

// chatRequest is the part of the chat completions body the stub evaluates.
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// handleChat answers a chat completion, streamed or in one piece.
func (b *Backend) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	model := req.Model
	if model == "" {
		model = "gpt-5.5"
	}
	answer := reply(b.lang)
	if b.isTitleRequest(req) {
		answer = b.title()
	}

	var promptChars int
	for _, m := range req.Messages {
		promptChars += len(m.Content)
	}
	usage := map[string]int{
		"prompt_tokens":     promptChars / 4,
		"completion_tokens": len(answer) / 4,
		"total_tokens":      promptChars/4 + len(answer)/4,
	}

	if !req.Stream {
		writeJSON(w, map[string]any{
			"model": model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": answer},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	send := func(chunk map[string]any) bool {
		data, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Token by token, so the interface shows a real stream.
	for _, token := range strings.SplitAfter(answer, " ") {
		if token == "" {
			continue
		}
		if !send(map[string]any{
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": token}}},
		}) {
			return // client disconnected
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(b.streamDelay):
		}
	}
	send(map[string]any{
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
	})
	send(map[string]any{"model": model, "choices": []map[string]any{}, "usage": usage})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// isTitleRequest detects the internal request that names a chat. Its system
// prompt is the only one that asks for a title.
func (b *Backend) isTitleRequest(req chatRequest) bool {
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		lower := strings.ToLower(m.Content)
		if strings.Contains(lower, "title") || strings.Contains(lower, "titel") {
			return true
		}
	}
	return false
}

// title is the chat title the stub returns for the demo conversation.
func (b *Backend) title() string {
	if b.lang == "de" {
		return "Sicheres Self-Hosting"
	}
	return "Hosting this app securely"
}

// handleEmbeddings returns the same deterministic vectors the seeded chunks use.
func (b *Backend) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input []string `json:"input"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := make([]map[string]any, 0, len(req.Input))
	tokens := 0
	for i, text := range req.Input {
		data = append(data, map[string]any{"index": i, "embedding": Embedding(text)})
		tokens += len(text) / 4
	}
	writeJSON(w, map[string]any{
		"data":  data,
		"usage": map[string]int{"prompt_tokens": tokens, "total_tokens": tokens},
	})
}

// handleImage renders a prompt into the demo illustration.
func (b *Backend) handleImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt       string `json:"prompt"`
		OutputFormat string `json:"output_format"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.writeImage(w, req.Prompt, req.OutputFormat)
}

// handleImageEdit answers the multipart edit request with a new variant.
func (b *Backend) handleImageEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	// #nosec G120 -- the request body is bounded by MaxBytesReader above.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	b.writeImage(w, r.FormValue("prompt"), r.FormValue("output_format"))
}

// writeImage encodes the illustration for a prompt as the endpoints do.
func (b *Backend) writeImage(w http.ResponseWriter, prompt, format string) {
	data, _, err := encodeImage(renderIllustration(1536, 1024, accentFor(prompt)), format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(data)}},
		"usage": map[string]int{
			"input_tokens":  len(prompt) / 4,
			"output_tokens": 1580,
			"total_tokens":  len(prompt)/4 + 1580,
		},
	})
}

// decodeJSON reads a size limited JSON body.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(dst)
}

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Debug("demo backend response", "err", err)
	}
}
