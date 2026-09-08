package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

type responseInput struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    *string         `json:"output,omitempty"`
}

type responseContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type responseReasoning struct {
	Effort string `json:"effort"`
}

type responseRequest struct {
	Model             string             `json:"model"`
	Input             []json.RawMessage  `json:"input"`
	Tools             []responseTool     `json:"tools,omitempty"`
	ToolChoice        string             `json:"tool_choice"`
	ParallelToolCalls bool               `json:"parallel_tool_calls"`
	Reasoning         *responseReasoning `json:"reasoning,omitempty"`
	Stream            bool               `json:"stream"`
	Store             bool               `json:"store"`
	Include           []string           `json:"include"`
}

type completedResponse struct {
	Status string            `json:"status"`
	Model  string            `json:"model"`
	Output []json.RawMessage `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) useResponses(opts ChatOptions, messages []Message, tools bool) bool {
	for _, message := range messages {
		if len(message.ResponseItems) > 0 {
			return true
		}
	}
	if !tools {
		return false
	}
	model := strings.ToLower(c.store.ModelIdentity(chatDeployment(c.store.Get(), opts.Model)))
	return strings.HasPrefix(model, "gpt-6-") || strings.HasPrefix(model, "gpt-5.6-") ||
		model == "gpt-5.5" || strings.HasPrefix(model, "gpt-5.5-")
}

func responseInputs(messages []Message) ([]json.RawMessage, error) {
	var inputs []json.RawMessage
	appendInput := func(input responseInput) error {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		inputs = append(inputs, data)
		return nil
	}
	for _, message := range messages {
		if len(message.ResponseItems) > 0 {
			inputs = append(inputs, message.ResponseItems...)
			continue
		}
		if message.Role == "tool" {
			if message.ToolCallID == "" {
				return nil, fmt.Errorf("tool result has no call ID")
			}
			if err := appendInput(responseInput{
				Type: "function_call_output", CallID: message.ToolCallID, Output: &message.Content,
			}); err != nil {
				return nil, err
			}
			continue
		}
		if message.Content != "" || len(message.Images) > 0 {
			content, err := json.Marshal(message.Content)
			if err != nil {
				return nil, err
			}
			if len(message.Images) > 0 {
				var parts []responseContent
				if message.Content != "" {
					parts = append(parts, responseContent{Type: "input_text", Text: message.Content})
				}
				for _, image := range message.Images {
					parts = append(parts, responseContent{Type: "input_image", ImageURL: dataURL(image.MIME, image.Data)})
				}
				content, err = json.Marshal(parts)
				if err != nil {
					return nil, err
				}
			}
			if err := appendInput(responseInput{Type: "message", Role: message.Role, Content: content}); err != nil {
				return nil, err
			}
		}
		for _, call := range message.ToolCalls {
			if err := appendInput(responseInput{
				Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
			}); err != nil {
				return nil, err
			}
		}
	}
	return inputs, nil
}

func (c *Client) responsesTurn(ctx context.Context, opts ChatOptions, messages []Message, tools []Tool, onDelta func(string) error) (TurnResult, error) {
	var result TurnResult
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || !c.store.HasChatCredentials() {
		return result, fmt.Errorf("chat endpoint, deployment and credentials are required")
	}
	inputs, err := responseInputs(messages)
	if err != nil {
		return result, err
	}
	request := responseRequest{
		Model: chatDeployment(cfg, opts.Model), Input: inputs, Stream: true, Store: false,
		Include: []string{"reasoning.encrypted_content"}, ToolChoice: "none",
	}
	for _, tool := range tools {
		if tool.Type != "function" {
			return result, fmt.Errorf("unsupported response tool type %q", tool.Type)
		}
		request.Tools = append(request.Tools, responseTool{
			Type: "function", Name: tool.Function.Name, Description: tool.Function.Description,
			Parameters: tool.Function.Parameters, Strict: false,
		})
	}
	if len(request.Tools) > 0 {
		request.ToolChoice = "auto"
	}
	effort := NormalizeReasoningEffort(c.store.ModelIdentity(request.Model), opts.ReasoningEffort)
	if effort = optionValue(effort); effort != "" {
		request.Reasoning = &responseReasoning{Effort: effort}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	if !IsV1Endpoint(base) {
		base += "/openai/v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/responses", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if err := c.store.Authorize(req, foundry.Chat, request.Model); err != nil {
		return result, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }() // The response is consumed or its error is returned.
	if resp.StatusCode != http.StatusOK {
		return result, readError(resp)
	}
	var text strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string            `json:"type"`
			Delta    string            `json:"delta"`
			Message  string            `json:"message"`
			Response completedResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return result, fmt.Errorf("decode responses stream: %w", err)
		}
		switch event.Type {
		case "response.output_text.delta", "response.refusal.delta":
			text.WriteString(event.Delta)
			if err := onDelta(event.Delta); err != nil {
				return result, err
			}
		case "response.completed", "response.incomplete", "response.failed":
			result.Content = text.String()
			result.Model = event.Response.Model
			result.Usage = Usage{
				PromptTokens: event.Response.Usage.InputTokens, CompletionTokens: event.Response.Usage.OutputTokens,
				TotalTokens: event.Response.Usage.TotalTokens,
			}
			c.metrics.recordChat(result.Usage)
			if c.recorder != nil && result.Usage.TotalTokens > 0 {
				c.recorder.RecordUsage("chat", result.Model, result.Usage)
			}
			if event.Type != "response.completed" || event.Response.Status != "completed" {
				if event.Response.Error != nil {
					return result, fmt.Errorf("responses request failed: %s", event.Response.Error.Message)
				}
				return result, fmt.Errorf("responses request did not complete: %s", event.Response.Status)
			}
			seen := make(map[string]bool)
			for _, raw := range event.Response.Output {
				var item struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}
				if err := json.Unmarshal(raw, &item); err != nil {
					return result, fmt.Errorf("decode response output item: %w", err)
				}
				if item.Type == "function_call" {
					if item.CallID == "" || item.Name == "" || seen[item.CallID] {
						return result, fmt.Errorf("response contains an invalid function call")
					}
					seen[item.CallID] = true
					result.ToolCalls = append(result.ToolCalls, ToolCall{
						ID: item.CallID, Type: "function", Function: ToolCallFunction{Name: item.Name, Arguments: item.Arguments},
					})
				}
			}
			// Replay the full output, including encrypted reasoning, only within this tool loop.
			result.ResponseItems = event.Response.Output
			result.FinishReason = "stop"
			if len(result.ToolCalls) > 0 {
				result.FinishReason = "tool_calls"
			}
			return result, nil
		case "error":
			return result, fmt.Errorf("responses stream error: %s", event.Message)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	return result, fmt.Errorf("responses stream ended before completion")
}
