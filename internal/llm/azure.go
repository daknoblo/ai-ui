// Package llm talks to the Azure-OpenAI-compatible model router.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
)

// maxErrorBodyBytes limits how much of an error response is read before it is
// turned into a Go error. Endpoints can return very large HTML error pages.
const maxErrorBodyBytes = 8 << 10

// Message is a chat message in OpenAI format. Tool calling uses the additional
// fields (ToolCalls for assistant requests, ToolCallID/Name for tool results).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
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
	Model         string         `json:"model,omitempty"`
	Messages      []Message      `json:"messages"`
	Temperature   float64        `json:"temperature"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
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
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Model        string
}

// isV1Endpoint detects the new OpenAI-compatible v1 schema of Azure AI Foundry
// by the "/openai/v1" path segment (e.g.
// https://resource.services.ai.azure.com/openai/v1). With that schema the
// standard OpenAI paths are appended and the deployment is passed in the
// "model" field of the request body instead of in the URL path. Otherwise the
// classic Azure OpenAI schema applies
// (/openai/deployments/{deployment}/...?api-version=...).
func isV1Endpoint(endpoint string) bool {
	return strings.Contains(strings.TrimRight(endpoint, "/"), "/openai/v1")
}

// chatCompletionsURL builds the chat completions URL for the endpoint schema.
func chatCompletionsURL(endpoint, deployment, apiVersion string) string {
	base := strings.TrimRight(endpoint, "/")
	if isV1Endpoint(base) {
		return base + "/chat/completions"
	}
	return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", base, deployment, apiVersion)
}

// embeddingsURL builds the embeddings URL for the endpoint schema.
func embeddingsURL(endpoint, deployment, apiVersion string) string {
	base := strings.TrimRight(endpoint, "/")
	if isV1Endpoint(base) {
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
	if cfg.ChatModel != "" {
		return cfg.ChatModel
	}
	if isV1Endpoint(cfg.Endpoint) {
		return cfg.ChatDeployment
	}
	return ""
}

// ChatStream sends the messages and calls onDelta for every text token. An empty
// model falls back to the configured one. When finished it returns the token
// usage and the model that was actually used.
func (c *Client) ChatStream(ctx context.Context, model string, messages []Message, onDelta func(string) error) (ChatResult, error) {
	turn, err := c.streamTurn(ctx, model, messages, nil, onDelta)
	return ChatResult{Usage: turn.Usage, Model: turn.Model}, err
}

// ChatStreamWithTools behaves like ChatStream but offers the given tools to the
// model and returns the tool calls it requested, if any.
func (c *Client) ChatStreamWithTools(ctx context.Context, model string, messages []Message, tools []Tool, onDelta func(string) error) (TurnResult, error) {
	return c.streamTurn(ctx, model, messages, tools, onDelta)
}

// streamTurn runs one streaming pass, streams text through onDelta and collects
// optional tool calls (whose arguments arrive across several chunks).
func (c *Client) streamTurn(ctx context.Context, model string, messages []Message, tools []Tool, onDelta func(string) error) (TurnResult, error) {
	var result TurnResult
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || cfg.APIVersion == "" {
		return result, fmt.Errorf("incomplete configuration: endpoint, chat deployment and api version are required")
	}
	if !c.store.HasAPIKey() {
		return result, fmt.Errorf("no API key set (AZURE_API_KEY)")
	}

	url := chatCompletionsURL(cfg.Endpoint, cfg.ChatDeployment, cfg.APIVersion)

	reqBody := chatRequest{
		Model:         chatModelField(cfg, model),
		Messages:      messages,
		Temperature:   cfg.Temperature,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if len(tools) > 0 {
		reqBody.Tools = tools
		reqBody.ToolChoice = "auto"
	}
	slog.Debug("chat request", "url", url, "model", reqBody.Model, "messages", len(messages), "tools", len(tools))
	body, err := json.Marshal(reqBody)
	if err != nil {
		return result, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.store.APIKey())
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return result, readError(resp)
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
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || cfg.APIVersion == "" {
		return fmt.Errorf("endpoint, chat deployment and api version are required")
	}
	if !c.store.HasAPIKey() {
		return fmt.Errorf("no API key set (AZURE_API_KEY)")
	}

	url := chatCompletionsURL(cfg.Endpoint, cfg.ChatDeployment, cfg.APIVersion)

	body, err := json.Marshal(chatRequest{
		Model:     chatModelField(cfg, ""),
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
	req.Header.Set("api-key", c.store.APIKey())

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

// responseMentionsMaxTokens reports whether an error response hints at an
// exhausted token limit (in which case the endpoint itself is reachable).
func responseMentionsMaxTokens(resp *http.Response) bool {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, maxErrorBodyBytes))
	msg := strings.ToLower(buf.String())
	return strings.Contains(msg, "max_tokens") || strings.Contains(msg, "output limit")
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
	cfg := c.store.Get()
	if cfg.EmbeddingDeployment == "" {
		return nil, fmt.Errorf("no embedding deployment configured")
	}
	if !c.store.HasEmbeddingAPIKey() {
		return nil, fmt.Errorf("no API key set (AZURE_API_KEY or AZURE_EMBEDDING_API_KEY)")
	}

	url := embeddingsURL(cfg.EmbeddingHost(), cfg.EmbeddingDeployment, cfg.EmbeddingVersion())

	body, err := json.Marshal(embeddingRequest{Input: inputs})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.store.EmbeddingAPIKey())

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
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, maxErrorBodyBytes))
	msg := strings.TrimSpace(buf.String())
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return fmt.Errorf("azure error %d: %s", resp.StatusCode, msg)
}
