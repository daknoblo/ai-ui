// Package llm talks to the Azure-OpenAI-compatible model router.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
)

// maxErrorBodyBytes limits how much of an error response is read before it is
// turned into a Go error. Endpoints can return very large HTML error pages.
const maxErrorBodyBytes = 8 << 10

// Message is a chat message in OpenAI format. Tool calling uses the additional
// fields (ToolCalls for assistant requests, ToolCallID/Name for tool results).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images are attachments the model should look at. They are not a field of
	// the wire format: MarshalJSON turns them into multimodal content parts.
	Images        []ImageContent    `json:"-"`
	ToolCalls     []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID    string            `json:"tool_call_id,omitempty"`
	Name          string            `json:"name,omitempty"`
	ResponseItems []json.RawMessage `json:"-"`
}

// ImageContent is an image attached to a message. It is sent inline as a data
// URL, so no endpoint has to be able to reach this instance.
type ImageContent struct {
	MIME string
	Data []byte
}

// contentPart is one element of a multimodal content array.
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

// imageURLPart carries the data URL of an attached image.
type imageURLPart struct {
	URL string `json:"url"`
}

// MarshalJSON writes a message with attachments as a multimodal content array.
// Without attachments the content stays a plain string, so the request body is
// unchanged for endpoints that do not implement the vision schema.
func (m Message) MarshalJSON() ([]byte, error) {
	// plain drops the method set, which would otherwise recurse into this one.
	type plain Message

	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}

	parts := make([]contentPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, contentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, contentPart{
			Type:     "image_url",
			ImageURL: &imageURLPart{URL: dataURL(img.MIME, img.Data)},
		})
	}
	// The outer Content sits at a shallower depth than the embedded one, so it
	// replaces the string variant instead of colliding with it.
	return json.Marshal(struct {
		plain
		Content []contentPart `json:"content"`
	}{plain: plain(m), Content: parts})
}

// dataURL encodes image bytes as an inline data URL.
func dataURL(mime string, data []byte) string {
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ToolCall describes a function call requested by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the name and JSON arguments of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool defines a tool offered to the model (function calling).
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function including its JSON schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Client talks to the Azure-OpenAI-compatible model router.
type Client struct {
	store    *config.Store
	http     *http.Client
	metrics  *Metrics
	recorder UsageRecorder
}

// UsageRecorder receives every token usage for persistent storage.
type UsageRecorder interface {
	RecordUsage(kind, model string, u Usage)
}

// New creates a new LLM client. The transport keeps idle connections around so
// that consecutive chat and embedding calls reuse the TLS session instead of
// performing a full handshake each time.
func New(store *config.Store) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 90 * time.Second
	transport.ForceAttemptHTTP2 = true

	return &Client{
		store: store,
		http: &http.Client{
			// Long timeout: a streamed answer can legitimately take minutes.
			Timeout:   5 * time.Minute,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 || (len(via) > 0 && req.URL.Host != via[0].URL.Host) {
					return fmt.Errorf("inference redirect is not allowed")
				}
				return nil
			},
		},
		metrics: &Metrics{},
	}
}

// SetUsageRecorder registers a receiver for the persistent usage statistics.
func (c *Client) SetUsageRecorder(r UsageRecorder) {
	c.recorder = r
}

// Metrics returns a snapshot of the cumulative token usage.
func (c *Client) Metrics() MetricsSnapshot {
	return c.metrics.Snapshot()
}

// Usage describes the token usage of a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// streamOptions enables usage reporting at the end of a stream.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatRequest is the request body for chat completions.
type chatRequest struct {
	Model           string         `json:"model,omitempty"`
	Messages        []Message      `json:"messages"`
	Temperature     *float64       `json:"temperature,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	MaxTokens       int            `json:"max_completion_tokens,omitempty"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolChoice      string         `json:"tool_choice,omitempty"`
}

// dropRejected removes the parameter an error message complains about and
// returns its name. An empty name means there is nothing left to retry.
func (r *chatRequest) dropRejected(msg string) string {
	switch {
	case r.Temperature != nil && strings.Contains(msg, "temperature"):
		r.Temperature = nil
		return "temperature"
	case r.ReasoningEffort != "" && strings.Contains(msg, "reasoning"):
		r.ReasoningEffort = ""
		return "reasoning_effort"
	}
	return ""
}

// streamChunk is a single SSE delta of the chat completions response.
type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// ChatResult bundles the metadata of a finished chat answer.
type ChatResult struct {
	Usage Usage
	Model string // model actually used (as reported by the router)
}

// TurnResult is the outcome of a single stream pass including the tool calls
// the model requested, if any.
type TurnResult struct {
	Content       string
	ToolCalls     []ToolCall
	FinishReason  string
	Usage         Usage
	Model         string
	ResponseItems []json.RawMessage
}

// IsV1Endpoint detects the new OpenAI-compatible v1 schema of Azure AI Foundry
// by the "/openai/v1" path segment (e.g.
// https://resource.services.ai.azure.com/openai/v1). With that schema the
// standard OpenAI paths are appended and the deployment is passed in the
// "model" field of the request body instead of in the URL path. Otherwise the
// classic Azure OpenAI schema applies
// (/openai/deployments/{deployment}/...?api-version=...).
func IsV1Endpoint(endpoint string) bool {
	return foundry.IsV1Endpoint(endpoint)
}

// apiVersionFor resolves the api-version of a request. On the v1 surface the
// dated versions of the classic schema are meaningless, and only the preview
// moniker exposes the full Foundry model catalog, so that is the default there.
func apiVersionFor(endpoint, configured string) string {
	if !IsV1Endpoint(endpoint) {
		return configured
	}
	if configured == "preview" || configured == "v1" {
		return configured
	}
	return previewAPIVersion
}

// chatCompletionsURL builds the chat completions URL for the endpoint schema.
func chatCompletionsURL(endpoint, deployment, apiVersion string) string {
	base := strings.TrimRight(endpoint, "/")
	if IsV1Endpoint(base) {
		return base + "/chat/completions"
	}
	return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", base, deployment, apiVersion)
}

// embeddingsURL builds the embeddings URL for the endpoint schema.
func embeddingsURL(endpoint, deployment, apiVersion string) string {
	base := strings.TrimRight(endpoint, "/")
	if IsV1Endpoint(base) {
		return base + "/embeddings"
	}
	return fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s", base, deployment, apiVersion)
}

// chatModelField returns the value for the "model" field of the request body.
// override wins over the configured model; with the v1 schema the deployment is
// required, while the classic schema treats an empty value as "router decides".
func chatModelField(cfg config.Config, override string) string {
	if override != "" {
		return override
	}
	if cfg.Foundry {
		return cfg.ChatDeployment
	}
	if cfg.ChatModel != "" {
		return cfg.ChatModel
	}
	if IsV1Endpoint(cfg.Endpoint) {
		return cfg.ChatDeployment
	}
	return ""
}

// chatDeployment resolves the deployment a request is routed to. With the
// classic schema it forms the URL path, so a model picked in the interface has
// to replace the configured default there as well - the names in AZURE_MODELS
// are deployments of the same resource.
func chatDeployment(cfg config.Config, override string) string {
	if name := chatModelField(cfg, override); name != "" {
		return name
	}
	return cfg.ChatDeployment
}

// ChatOptions are the settings of a single chat turn. They belong to the chat,
// not to the client, so every request can use its own model and effort.
type ChatOptions struct {
	Model           string // deployment name; empty leaves the choice to the router
	ReasoningEffort string // "" or "auto" leaves it to the model
}

// ChatStream sends the messages and calls onDelta for every text token. An empty
// model falls back to the configured one. When finished it returns the token
// usage and the model that was actually used.

// ChatStream streams an answer and reports the token usage of the turn.
func (c *Client) ChatStream(ctx context.Context, opts ChatOptions, messages []Message, onDelta func(string) error) (ChatResult, error) {
	turn, err := c.streamTurn(ctx, opts, messages, nil, onDelta)
	return ChatResult{Usage: turn.Usage, Model: turn.Model}, err
}

// ChatStreamWithTools behaves like ChatStream but offers the given tools to the
// model and returns the tool calls it requested, if any.
func (c *Client) ChatStreamWithTools(ctx context.Context, opts ChatOptions, messages []Message, tools []Tool, onDelta func(string) error) (TurnResult, error) {
	return c.streamTurn(ctx, opts, messages, tools, onDelta)
}

// streamTurn runs one streaming pass, streams text through onDelta and collects
// optional tool calls (whose arguments arrive across several chunks).
func (c *Client) streamTurn(ctx context.Context, opts ChatOptions, messages []Message, tools []Tool, onDelta func(string) error) (TurnResult, error) {
	if c.useResponses(opts, messages, len(tools) > 0) {
		return c.responsesTurn(ctx, opts, messages, tools, onDelta)
	}
	var result TurnResult
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || (!IsV1Endpoint(cfg.Endpoint) && cfg.APIVersion == "") {
		return result, fmt.Errorf("incomplete configuration: endpoint, chat deployment and api version are required")
	}
	if !c.store.HasChatCredentials() {
		return result, fmt.Errorf("no chat credentials configured")
	}

	url := chatCompletionsURL(cfg.Endpoint, chatDeployment(cfg, opts.Model), cfg.APIVersion)

	reqBody := chatRequest{
		Model:           chatModelField(cfg, opts.Model),
		Messages:        messages,
		Temperature:     &cfg.Temperature,
		ReasoningEffort: optionValue(opts.ReasoningEffort),
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
	}
	if len(tools) > 0 {
		reqBody.Tools = tools
		reqBody.ToolChoice = "auto"
	}
	slog.Debug("chat request", "url", url, "model", reqBody.Model, "messages", len(messages), "tools", len(tools))

	resp, err := c.postChat(ctx, url, reqBody)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg := errorBody(resp)
		// Temperature and reasoning effort are global settings, but support for
		// them differs per model: reasoning models accept only their default
		// temperature, models without reasoning reject the effort. A rejected
		// parameter is dropped and the request repeated instead of failing.
		for resp.StatusCode == http.StatusBadRequest {
			dropped := reqBody.dropRejected(msg)
			if dropped == "" {
				break
			}
			slog.Debug("model rejects a parameter, retrying without it", "model", reqBody.Model, "parameter", dropped)
			_ = resp.Body.Close()
			resp, err = c.postChat(ctx, url, reqBody)
			if err != nil {
				return result, err
			}
			msg = ""
			if resp.StatusCode != http.StatusOK {
				msg = errorBody(resp)
			}
		}
		if resp.StatusCode != http.StatusOK {
			if msg == "" {
				msg = errorBody(resp)
			}
			// The requested model is part of the message: with the v1 schema it
			// has to be a deployment name, the usual cause of a 404 here.
			return result, fmt.Errorf("model %q: azure error %d: %s", reqBody.Model, resp.StatusCode, msg)
		}
	}

	// Tool calls are accumulated per index (arguments arrive fragmented).
	toolAcc := map[int]*ToolCall{}
	var toolOrder []int
	var content strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // skip incomplete/empty lines
		}
		if chunk.Model != "" {
			result.Model = chunk.Model
		}
		if chunk.Usage != nil {
			result.Usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				result.FinishReason = *ch.FinishReason
			}
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if err := onDelta(ch.Delta.Content); err != nil {
					return result, err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := toolAcc[tc.Index]
				if !ok {
					acc = &ToolCall{Type: "function"}
					toolAcc[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Type != "" {
					acc.Type = tc.Type
				}
				if tc.Function.Name != "" {
					acc.Function.Name = tc.Function.Name
				}
				acc.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}

	result.Content = content.String()
	for _, idx := range toolOrder {
		result.ToolCalls = append(result.ToolCalls, *toolAcc[idx])
	}
	c.metrics.recordChat(result.Usage)
	if c.recorder != nil && result.Usage.TotalTokens > 0 {
		c.recorder.RecordUsage("chat", result.Model, result.Usage)
	}
	return result, nil
}

// VerifyChat performs a minimal request to check that the chat endpoint is
// reachable and answers with a valid response.
func (c *Client) VerifyChat(ctx context.Context) error {
	return c.VerifyDeployment(ctx, "")
}

// VerifyDeployment checks a single chat deployment the same way. An empty name
// uses the configured default, which is what VerifyChat does.
func (c *Client) VerifyDeployment(ctx context.Context, deployment string) error {
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || (!IsV1Endpoint(cfg.Endpoint) && cfg.APIVersion == "") {
		return fmt.Errorf("endpoint, chat deployment and api version are required")
	}
	if !c.store.HasChatCredentials() {
		return fmt.Errorf("no chat credentials configured")
	}

	url := chatCompletionsURL(cfg.Endpoint, chatDeployment(cfg, deployment), cfg.APIVersion)

	body, err := json.Marshal(chatRequest{
		Model:     chatModelField(cfg, deployment),
		Messages:  []Message{{Role: "user", Content: "ping"}},
		Stream:    false,
		MaxTokens: 16,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.store.Authorize(req, foundry.Chat, chatDeployment(cfg, deployment)); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// A 400 caused by an exhausted token budget still proves that endpoint,
		// deployment and authentication are correct, so it counts as success.
		if resp.StatusCode == http.StatusBadRequest && responseMentionsMaxTokens(resp) {
			return nil
		}
		return readError(resp)
	}
	return nil
}

// responseMentionsMaxTokens reports whether an error response hints at the
// token limit (in which case the endpoint itself is reachable). Model families
// disagree on the field name, so both spellings count.
func responseMentionsMaxTokens(resp *http.Response) bool {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, maxErrorBodyBytes))
	msg := strings.ToLower(buf.String())
	return strings.Contains(msg, "max_tokens") ||
		strings.Contains(msg, "max_completion_tokens") ||
		strings.Contains(msg, "output limit")
}

// VerifyEmbedding performs a minimal request to check that the embedding
// endpoint is reachable and returns a valid embedding.
func (c *Client) VerifyEmbedding(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	vecs, err := c.Embed(ctx, []string{"ping"})
	if err != nil {
		return err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return fmt.Errorf("no valid embedding received")
	}
	return nil
}

// embeddingRequest is the request body for the embeddings API.
type embeddingRequest struct {
	Model string   `json:"model,omitempty"`
	Input []string `json:"input"`
}

// embeddingResponse is the response of the embeddings API.
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage Usage `json:"usage"`
}

// Embed creates embeddings for the given texts.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return c.embed(ctx, c.store.Get(), inputs)
}

func (c *Client) embed(ctx context.Context, cfg config.Config, inputs []string) ([][]float32, error) {
	if cfg.EmbeddingDeployment == "" {
		return nil, fmt.Errorf("no embedding deployment configured")
	}
	if !c.store.HasEmbeddingCredentials() {
		return nil, fmt.Errorf("no embedding credentials configured")
	}

	url := embeddingsURL(cfg.EmbeddingHost(), cfg.EmbeddingDeployment, cfg.EmbeddingVersion())

	reqBody := embeddingRequest{Input: inputs}
	// The v1 surface has no deployment in the path, so it travels in the body.
	if IsV1Endpoint(cfg.EmbeddingHost()) {
		reqBody.Model = cfg.EmbeddingDeployment
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.store.Authorize(req, foundry.Embeddings, cfg.EmbeddingDeployment); err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}

	var out embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	c.metrics.recordEmbedding(out.Usage.TotalTokens)
	if c.recorder != nil && out.Usage.TotalTokens > 0 {
		c.recorder.RecordUsage("embedding", cfg.EmbeddingDeployment, out.Usage)
	}

	result := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(result) {
			result[d.Index] = d.Embedding
		}
	}
	return result, nil
}

// readError reads an error response and formats it.
func readError(resp *http.Response) error {
	return fmt.Errorf("azure error %d: %s", resp.StatusCode, errorBody(resp))
}

// errorBody reads and shortens the body of an error response.
func errorBody(resp *http.Response) string {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, maxErrorBodyBytes))
	msg := strings.TrimSpace(buf.String())
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

// postChat sends a chat completions request.
func (c *Client) postChat(ctx context.Context, url string, reqBody chatRequest) (*http.Response, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.store.Authorize(req, foundry.Chat, reqBody.Model); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	return c.http.Do(req)
}
