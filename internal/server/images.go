package server

import (
	"context"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// allowedImageMIME lists the content types the image endpoint may produce or
// accept as a source. Serving is restricted to them so a manipulated database
// row cannot turn a stored blob into an active content type.
var allowedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// uploadImageMIME returns the content type when an upload is a supported image,
// otherwise an empty string. The extension decides when the browser sends no
// usable type.
func uploadImageMIME(header *multipart.FileHeader) string {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(header.Header.Get("Content-Type"), ";")[0]))
	if allowedImageMIME[mime] {
		return mime
	}
	switch strings.ToLower(filepath.Ext(header.Filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// handleDeleteImage removes an uploaded source image.
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	chatID, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	imageID, err := strconv.ParseInt(chi.URLParam(r, "iid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteImage(r.Context(), imageID); err != nil {
		s.httpError(w, err)
		return
	}
	s.renderDocList(w, r, chatID, "", false)
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

// handleSetImageParams stores the generation parameters chosen in the composer.
// Like the model picker they are global and survive switching chats.
func (s *Server) handleSetImageParams(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	cfg := s.cfg.Get()
	cfg.ImageSize = imageParam(r.FormValue("image_size"), imageSizes)
	cfg.ImageQuality = imageParam(r.FormValue("image_quality"), imageQualities)
	cfg.ImageFormat = imageParam(r.FormValue("image_format"), imageFormats)
	if err := s.cfg.Save(cfg); err != nil {
		s.httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// generateImage renders the prompt into an image, stores it and pushes it into
// the open SSE stream. With edit the latest image of the chat is modified
// instead of creating a new one, which allows refining step by step.
func (s *Server) generateImage(ctx context.Context, sse *sseWriter, chatID int64, prompt string, edit bool, fail func(string)) {
	cfg := s.cfg.Get()
	opts := llm.ImageOptions{
		Size:    cfg.ImageSize,
		Quality: cfg.ImageQuality,
		Format:  cfg.ImageFormat,
	}

	// Editing continues from the latest image, so refinements build on each other.
	var src *storage.Image
	if edit {
		if img, lookupErr := s.store.LatestImage(ctx, chatID); lookupErr == nil {
			src = &img
		}
	}

	var (
		res llm.ImageResult
		err error
	)
	if src != nil {
		res, err = s.llm.EditImage(ctx, prompt, llm.ImageSource{
			Name: src.Name,
			MIME: src.MIME,
			Data: src.Data,
		}, opts)
	} else {
		res, err = s.llm.GenerateImage(ctx, prompt, opts)
	}
	if err != nil {
		slog.Error("image generation", "edit", src != nil, "err", err)
		fail(s.t("stream.image_failed", err.Error()))
		return
	}

	// Deliberately not the request context: the result must be persisted even
	// when the browser has already navigated away.
	imageID, err := s.store.AddImage(context.Background(), chatID,
		storage.ImageGenerated, "", prompt, res.MIME, res.Data)
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
	imageSizes     = []string{"auto", "1024x1024", "1024x1536", "1536x1024"}
	imageQualities = []string{"auto", "high", "medium", "low"}
	imageFormats   = []string{"png", "jpeg"}
)
