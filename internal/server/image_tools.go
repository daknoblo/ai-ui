package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/daknoblo/ai-ui/internal/llm"
)

func (s *Server) imageTool() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "generate_image",
		Description: s.t("prompt.image_tool_description"),
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"prompt":{"type":"string","minLength":1,"maxLength":32768},"edit":{"type":"boolean"}},
			"required":["prompt","edit"],
			"additionalProperties":false
		}`),
	}}
}

func (s *Server) executeImageTool(ctx context.Context, sse *generationStream, chatID int64, call llm.ToolCall, routingUsage llm.Usage, fail func(string)) error {
	var args struct {
		Prompt string `json:"prompt"`
		Edit   bool   `json:"edit"`
	}
	decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return fmt.Errorf("invalid image tool arguments: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("image tool arguments must contain exactly one JSON object")
	}
	args.Prompt = strings.TrimSpace(args.Prompt)
	if args.Prompt == "" || len(args.Prompt) > 32<<10 {
		return fmt.Errorf("image tool prompt is empty or exceeds the size limit")
	}
	if !s.cfg.ImagesConfigured() {
		return fmt.Errorf("no image model configured")
	}
	// The image API, not this chat model, handles the user's generation/edit request.
	if err := sse.send("tool", s.renderString("turn-note", s.t("tool.generating_image"))); err != nil {
		return err
	}
	s.generateImage(ctx, sse, chatID, args.Prompt, args.Edit, routingUsage, fail)
	return nil
}
