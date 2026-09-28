package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

var FluxSizes = []string{"auto", "1024x1024", "1024x1536", "1536x1024", "2048x2048", "1536x2560", "2560x1536"}

func isFlux(model string) bool { return model == "flux.2-pro" || model == "flux.2-flex" }

// PrepareImage freezes a replica and its credential before a durable turn starts.
func (c *Client) PrepareImage(opts ImageOptions, edit bool) (ImageOptions, error) {
	if opts.route != nil {
		return opts, nil
	}
	if c.store.ImageFoundryStatus().Enabled {
		op := foundry.Images
		if edit {
			op = foundry.ImageEdits
		}
		bound, deployment, err := c.store.SelectImageRoute(op, opts.Deployment)
		if err != nil {
			return opts, err
		}
		routed := *c
		routed.store = bound
		opts.route = &routed
		opts.Deployment, opts.Model = deployment.Name, deployment.CanonicalModel()
	} else {
		opts.Deployment = imageDeployment(c.store.Get(), opts.Deployment)
		opts.Model = strings.ToLower(opts.Deployment)
		// Freeze manual endpoints and keys too; live settings cannot redirect
		// a queued request after the user submits it.
		routed := *c
		routed.store = c.store.Snapshot()
		opts.route = &routed
	}
	if isFlux(opts.Model) {
		opts.Size = opts.FluxSize
		opts.Quality = ""
	}
	if err := ValidateImageOptions(opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func ValidateImageOptions(opts ImageOptions) error {
	if opts.Format != "" && opts.Format != "png" && opts.Format != "jpeg" {
		return fmt.Errorf("unsupported image output format")
	}
	if isFlux(opts.Model) {
		if opts.Size != "" && !slices.Contains(FluxSizes, opts.Size) {
			return fmt.Errorf("unsupported FLUX image resolution")
		}
		if opts.Steps < 0 || opts.Steps > 50 ||
			math.IsNaN(opts.Guidance) || math.IsInf(opts.Guidance, 0) ||
			(opts.Guidance != 0 && (opts.Guidance < 1.5 || opts.Guidance > 10)) {
			return fmt.Errorf("invalid FLUX flex steps or guidance")
		}
		return nil
	}
	if opts.Size != "" && !slices.Contains([]string{"auto", "1024x1024", "1024x1536", "1536x1024"}, opts.Size) {
		return fmt.Errorf("unsupported GPT image resolution")
	}
	if opts.Quality != "" && !slices.Contains([]string{"auto", "low", "medium", "high"}, opts.Quality) {
		return fmt.Errorf("unsupported GPT image quality")
	}
	return nil
}

func (c *Client) fluxImage(ctx context.Context, prompt string, src *ImageSource, opts ImageOptions) (ImageResult, error) {
	path := strings.Replace(opts.Model, "flux.2-", "flux-2-", 1)
	endpoint, err := foundry.FluxURL(c.store.Get().ImageHost(), path)
	if err != nil {
		return ImageResult{}, err
	}
	payload := struct {
		Model        string  `json:"model"`
		Prompt       string  `json:"prompt"`
		NumImages    int     `json:"num_images"`
		Width        int     `json:"width,omitempty"`
		Height       int     `json:"height,omitempty"`
		OutputFormat string  `json:"output_format"`
		InputImage   string  `json:"input_image,omitempty"`
		Steps        int     `json:"steps,omitempty"`
		Guidance     float64 `json:"guidance,omitempty"`
	}{Model: opts.Deployment, Prompt: prompt, NumImages: 1, OutputFormat: opts.Format}
	if payload.OutputFormat == "" {
		payload.OutputFormat = "png"
	}
	if size := optionValue(opts.Size); size != "" {
		parts := strings.Split(size, "x")
		payload.Width, err = strconv.Atoi(parts[0])
		if err != nil {
			return ImageResult{}, err
		}
		payload.Height, err = strconv.Atoi(parts[1])
		if err != nil {
			return ImageResult{}, err
		}
	}
	op := foundry.Images
	if src != nil {
		if len(src.Data) == 0 || len(src.Data) > maxImageBodyBytes ||
			!slices.Contains([]string{"image/png", "image/jpeg", "image/webp"}, src.MIME) {
			return ImageResult{}, fmt.Errorf("invalid FLUX source image")
		}
		payload.InputImage = base64.StdEncoding.EncodeToString(src.Data)
		op = foundry.ImageEdits
	}
	if opts.Model == "flux.2-flex" {
		payload.Steps, payload.Guidance = opts.Steps, opts.Guidance
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ImageResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ImageResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.store.Authorize(req, op, opts.Deployment); err != nil {
		return ImageResult{}, err
	}
	if !c.store.ImageFoundryStatus().Enabled {
		req.Header.Del("api-key")
		req.Header.Set("Authorization", "Bearer "+c.store.ImageAPIKey())
	}
	// Azure's BFL adapter returns data[].b64_json synchronously (the official
	// Foundry sample), unlike BFL's own public asynchronous API. Do not follow
	// polling or image URLs, nor forward resource credentials to such URLs.
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return ImageResult{}, err
	}
	defer func() { _ = resp.Body.Close() }() // The response is fully consumed or rejected.
	if resp.StatusCode != http.StatusOK {
		return ImageResult{}, readError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBodyBytes+1))
	if err != nil || len(raw) > maxImageBodyBytes {
		return ImageResult{}, fmt.Errorf("FLUX response unreadable or exceeds size limit")
	}
	var out imageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return ImageResult{}, fmt.Errorf("invalid FLUX image response: %w", err)
	}
	if len(out.Data) != 1 || out.Data[0].B64JSON == "" {
		return ImageResult{}, fmt.Errorf("FLUX response contains no inline image; URL and asynchronous responses are not supported")
	}
	data, err := base64.StdEncoding.DecodeString(out.Data[0].B64JSON)
	if err != nil || len(data) == 0 {
		return ImageResult{}, fmt.Errorf("invalid FLUX base64 image")
	}
	mime := http.DetectContentType(data)
	if mime != "image/png" && mime != "image/jpeg" {
		return ImageResult{}, fmt.Errorf("FLUX response is not a PNG or JPEG image")
	}
	usage := Usage{PromptTokens: out.Usage.InputTokens, CompletionTokens: out.Usage.OutputTokens, TotalTokens: out.Usage.TotalTokens}
	c.metrics.recordImage(usage)
	if c.recorder != nil {
		c.recorder.RecordUsage("image", opts.Deployment, usage)
	}
	return ImageResult{Model: opts.Deployment, Data: data, MIME: mime, Usage: usage}, nil
}
