package server

import (
	"context"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// crossResourceImageKey reports whether the image endpoint points at a different
// resource while no dedicated key is set. The inherited chat key is rejected
// there, which the endpoint answers with 401.
func crossResourceImageKey(cfg config.Config, hasOwnKey bool) bool {
	if hasOwnKey || cfg.ImageEndpoint == "" || cfg.Endpoint == "" {
		return false
	}
	return endpointHost(cfg.ImageEndpoint) != endpointHost(cfg.Endpoint)
}

// endpointHost reduces an endpoint URL to its host for comparison.
func endpointHost(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(raw)
	}
	return strings.ToLower(u.Host)
}

// allowedImageMIME lists the content types the image endpoint may produce or
// accept as a source. Serving is restricted to them so a manipulated database
// row cannot turn a stored blob into an active content type.
var allowedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// editableImageMIME is the subset the image edit endpoint accepts as a source.
// A GIF can be attached and looked at, but it cannot be edited.
var editableImageMIME = map[string]bool{
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
	case ".gif":
		return "image/gif"
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
	if err := s.store.DeleteImageForChat(r.Context(), chatID, imageID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
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
func (s *Server) generateImage(ctx context.Context, sse *generationStream, chatID int64, prompt string, edit bool, routingUsage llm.Usage, fail func(string)) {
	opts := sse.options.ImageOptions
	deployment := opts.Deployment

	// Editing continues from the latest image, so refinements build on each other.
	var src *storage.Image
	if edit {
		img, lookupErr := s.store.GetImage(ctx, sse.options.SourceImageID)
		if lookupErr != nil {
			slog.Warn("image edit source unavailable", "chat", chatID, "err", lookupErr)
			fail(s.t("stream.image_source_missing"))
			return
		}
		if !editableImageMIME[img.MIME] {
			fail(s.t("stream.image_format"))
			return
		}
		src = &img
	}

	var (
		res    llm.ImageResult
		genErr error
	)
	if src != nil {
		res, genErr = s.llm.EditImage(ctx, prompt, llm.ImageSource{
			Name: src.Name,
			MIME: src.MIME,
			Data: src.Data,
		}, opts)
	} else {
		res, genErr = s.llm.GenerateImage(ctx, prompt, opts)
	}
	if genErr != nil {
		slog.Error("image generation", "edit", src != nil, "deployment", deployment, "err", genErr)
		fail(s.t("stream.image_failed", genErr.Error()))
		return
	}

	// Blob, response message and terminal outcome are committed together by
	// the worker, even after the original browser has disconnected.
	sse.image = &storage.Image{Prompt: prompt, MIME: res.MIME, Data: res.Data}
	if deployment != "" {
		_ = sse.send("model", s.renderString("model-tag", deployment))
	}
	res.Usage.PromptTokens += routingUsage.PromptTokens
	res.Usage.CompletionTokens += routingUsage.CompletionTokens
	res.Usage.TotalTokens += routingUsage.TotalTokens
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
