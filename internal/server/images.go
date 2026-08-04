package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/daknoblo/ai-ui/internal/llm"
)

// allowedImageMIME lists the content types the image endpoint may produce.
// Serving is restricted to them so a manipulated database row cannot turn a
// stored blob into an active content type.
var allowedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// handleImage serves a generated image from the database.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	img, err := s.store.GetImage(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mime := img.MIME
	if !allowedImageMIME[mime] {
		mime = "image/png"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(img.Data)))
	// Images are immutable once generated.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if _, err := w.Write(img.Data); err != nil {
		slog.Warn("write image", "id", id, "err", err)
	}
}

// generateImage renders the prompt into an image, stores it and pushes it into
// the open SSE stream.
func (s *Server) generateImage(ctx context.Context, sse *sseWriter, chatID int64, prompt string, fail func(string)) {
	cfg := s.cfg.Get()
	res, err := s.llm.GenerateImage(ctx, prompt, llm.ImageOptions{
		Size:    cfg.ImageSize,
		Quality: cfg.ImageQuality,
		Format:  cfg.ImageFormat,
	})
	if err != nil {
		slog.Error("image generation", "err", err)
		fail(s.t("stream.image_failed", err.Error()))
		return
	}

	// Deliberately not the request context: the result must be persisted even
	// when the browser has already navigated away.
	imageID, err := s.store.AddImage(context.Background(), chatID, prompt, res.MIME, res.Data)
	if err != nil {
		slog.Error("save image", "err", err)
		fail(s.t("stream.image_failed", err.Error()))
		return
	}

	content := imageMarkdown(imageID, prompt)
	if _, err := s.store.AddMessage(context.Background(), chatID, "assistant", content); err != nil {
		slog.Error("save assistant message", "err", err)
	}

	_ = sse.send("token", renderMarkdownString(content))
	if cfg.ImageDeployment != "" {
		_ = sse.send("model", s.renderString("model-tag", cfg.ImageDeployment))
	}
	if res.Usage.TotalTokens > 0 {
		_ = sse.send("usage", s.t("usage.footer",
			s.thousands(int64(res.Usage.TotalTokens)),
			s.thousands(int64(res.Usage.PromptTokens)),
			s.thousands(int64(res.Usage.CompletionTokens))))
	}
	_ = sse.send("done", "")
}

// imageMarkdown builds the message content of a generated image. The prompt is
// only used as alt text, so the characters that carry meaning in a Markdown
// image are removed.
func imageMarkdown(imageID int64, prompt string) string {
	alt := strings.NewReplacer("[", "", "]", "", "(", "", ")", "", "\n", " ", "\r", " ").Replace(prompt)
	alt = strings.TrimSpace(truncateRunes(alt, 120))
	return "![" + alt + "](/images/" + strconv.FormatInt(imageID, 10) + ")"
}

// imageParam keeps the generation parameters within the values the endpoint
// accepts; anything unknown falls back to the first entry.
func imageParam(value string, allowed []string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, a := range allowed {
		if value == a {
			return value
		}
	}
	return allowed[0]
}

// Selectable generation parameters (the first entry is the fallback).
var (
	imageSizes     = []string{"1024x1024", "1024x1536", "1536x1024", "auto"}
	imageQualities = []string{"high", "medium", "low", "auto"}
	imageFormats   = []string{"png", "jpeg", "webp"}
)
