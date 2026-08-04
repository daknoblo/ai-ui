package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
)

// maxImageBodyBytes caps the response of an image request. A base64 encoded
// image of the supported sizes stays far below this limit.
const maxImageBodyBytes = 32 << 20

// previewAPIVersion is the api-version the image models are served under on the
// v1 surface. Without it the endpoint rejects them with "unknown_model".
const previewAPIVersion = "preview"

// imagesURL builds the image generation URL for the endpoint schema. Unlike
// chat completions, the v1 surface requires an api-version here as well.
func imagesURL(endpoint, deployment, apiVersion string) string {
	base := strings.TrimRight(endpoint, "/")
	if isV1Endpoint(base) {
		return base + "/images/generations?api-version=" + url.QueryEscape(apiVersion)
	}
	return fmt.Sprintf("%s/openai/deployments/%s/images/generations?api-version=%s", base, deployment, apiVersion)
}

// imageEditsURL builds the image edit URL for the endpoint schema.
func imageEditsURL(endpoint, deployment, apiVersion string) string {
	base := strings.TrimRight(endpoint, "/")
	if isV1Endpoint(base) {
		return base + "/images/edits?api-version=" + url.QueryEscape(apiVersion)
	}
	return fmt.Sprintf("%s/openai/deployments/%s/images/edits?api-version=%s", base, deployment, apiVersion)
}

// imageAPIVersion resolves the api-version of an image request. A v1 endpoint
// defaults to the preview surface instead of inheriting the dated chat version.
func imageAPIVersion(cfg config.Config) string {
	if cfg.ImageAPIVersion != "" {
		return cfg.ImageAPIVersion
	}
	return apiVersionFor(cfg.ImageHost(), cfg.APIVersion)
}

// ImageOptions are the generation parameters offered in the settings dialog.
// Empty values and "auto" are omitted so the service default applies.
type ImageOptions struct {
	Size    string // e.g. 1024x1024
	Quality string // low | medium | high
	Format  string // png | jpeg
}

// ImageResult is a generated image including its token usage.
type ImageResult struct {
	Data  []byte
	MIME  string
	Usage Usage
}

// imageRequest is the request body for image generations.
type imageRequest struct {
	Model        string `json:"model,omitempty"`
	Prompt       string `json:"prompt"`
	N            int    `json:"n"`
	Size         string `json:"size,omitempty"`
	Quality      string `json:"quality,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
}

// imageResponse is the response of the image generation endpoint. Azure returns
// the image inline as base64 for the gpt-image models.
type imageResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
	} `json:"data"`
	// The image endpoints report input/output tokens instead of the
	// prompt/completion naming used by chat completions.
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// GenerateImage renders a prompt into a single image.
func (c *Client) GenerateImage(ctx context.Context, prompt string, opts ImageOptions) (ImageResult, error) {
	cfg := c.store.Get()
	endpoint := cfg.ImageHost()
	if endpoint == "" || cfg.ImageDeployment == "" {
		return ImageResult{}, fmt.Errorf("image endpoint and deployment are required")
	}
	if !c.store.HasImageAPIKey() {
		return ImageResult{}, fmt.Errorf("no API key set (AZURE_IMAGE_API_KEY)")
	}

	reqBody := imageRequest{
		Prompt:       prompt,
		N:            1,
		Size:         optionValue(opts.Size),
		Quality:      optionValue(opts.Quality),
		OutputFormat: optionValue(opts.Format),
	}
	// With the v1 schema the deployment travels in the body; the classic schema
	// carries it in the path.
	if isV1Endpoint(endpoint) {
		reqBody.Model = cfg.ImageDeployment
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return ImageResult{}, err
	}

	// Image generation is slow compared to a chat turn.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	url := imagesURL(endpoint, cfg.ImageDeployment, imageAPIVersion(cfg))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ImageResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.store.ImageAPIKey())

	return c.sendImageRequest(req, url, cfg.ImageDeployment, opts.Format)
}

// ImageSource is the image an edit request starts from.
type ImageSource struct {
	Name string
	MIME string
	Data []byte
}

// EditImage changes an existing image according to the prompt.
func (c *Client) EditImage(ctx context.Context, prompt string, src ImageSource, opts ImageOptions) (ImageResult, error) {
	cfg := c.store.Get()
	endpoint := cfg.ImageHost()
	if endpoint == "" || cfg.ImageDeployment == "" {
		return ImageResult{}, fmt.Errorf("image endpoint and deployment are required")
	}
	if !c.store.HasImageAPIKey() {
		return ImageResult{}, fmt.Errorf("no API key set (AZURE_IMAGE_API_KEY)")
	}
	if len(src.Data) == 0 {
		return ImageResult{}, fmt.Errorf("no source image")
	}

	// The edit endpoint expects multipart/form-data with the image as a file.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="image"; filename=%q`, sourceFileName(src)))
	header.Set("Content-Type", src.MIME)
	part, err := mw.CreatePart(header)
	if err != nil {
		return ImageResult{}, err
	}
	if _, err := part.Write(src.Data); err != nil {
		return ImageResult{}, err
	}

	fields := map[string]string{
		"prompt":        prompt,
		"n":             "1",
		"size":          optionValue(opts.Size),
		"quality":       optionValue(opts.Quality),
		"output_format": optionValue(opts.Format),
	}
	// With the v1 schema the deployment travels in the body; the classic schema
	// carries it in the path.
	if isV1Endpoint(endpoint) {
		fields["model"] = cfg.ImageDeployment
	}
	for name, value := range fields {
		if value == "" {
			continue
		}
		if err := mw.WriteField(name, value); err != nil {
			return ImageResult{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return ImageResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	url := imageEditsURL(endpoint, cfg.ImageDeployment, imageAPIVersion(cfg))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return ImageResult{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("api-key", c.store.ImageAPIKey())

	return c.sendImageRequest(req, url, cfg.ImageDeployment, opts.Format)
}

// sourceFileName keeps the extension the endpoint uses to detect the format.
func sourceFileName(src ImageSource) string {
	name := filepath.Base(strings.TrimSpace(src.Name))
	if name != "" && name != "." && name != string(filepath.Separator) && filepath.Ext(name) != "" {
		return name
	}
	switch strings.ToLower(strings.TrimSpace(src.MIME)) {
	case "image/jpeg", "image/jpg":
		return "source.jpg"
	case "image/webp":
		return "source.webp"
	default:
		return "source.png"
	}
}

// sendImageRequest performs an image request and decodes the base64 result.
func (c *Client) sendImageRequest(req *http.Request, url, deployment, format string) (ImageResult, error) {
	slog.Debug("image request", "url", url, "deployment", deployment, "content_type", req.Header.Get("Content-Type"))
	resp, err := c.http.Do(req)
	if err != nil {
		return ImageResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The URL is part of the message: a wrong endpoint or deployment is by
		// far the most common cause here.
		return ImageResult{}, fmt.Errorf("%s: %w", url, readError(resp))
	}

	var out imageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxImageBodyBytes)).Decode(&out); err != nil {
		return ImageResult{}, err
	}
	if len(out.Data) == 0 || out.Data[0].B64JSON == "" {
		return ImageResult{}, fmt.Errorf("the endpoint returned no image data")
	}
	raw, err := base64.StdEncoding.DecodeString(out.Data[0].B64JSON)
	if err != nil {
		return ImageResult{}, fmt.Errorf("decode image: %w", err)
	}

	usage := Usage{
		PromptTokens:     out.Usage.InputTokens,
		CompletionTokens: out.Usage.OutputTokens,
		TotalTokens:      out.Usage.TotalTokens,
	}
	c.metrics.recordImage(usage)
	if c.recorder != nil {
		c.recorder.RecordUsage("image", deployment, usage)
	}

	return ImageResult{Data: raw, MIME: imageMIME(format), Usage: usage}, nil
}

// optionValue drops empty and "auto" values so the service default applies.
func optionValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "auto") {
		return ""
	}
	return v
}

// imageMIME maps the requested output format to a content type. WEBP is not
// supported by the Azure image models.
func imageMIME(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	default:
		return "image/png"
	}
}
