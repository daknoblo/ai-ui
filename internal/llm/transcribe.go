package llm

import (
	"context"
	"fmt"
	"strings"
)

// TranscribeResult is the text read off a set of page images plus what it cost.
type TranscribeResult struct {
	Text  string
	Pages int // pages that produced text
	Model string
	Usage Usage
}

// CanTranscribe reports whether reading images as text is possible at all: it
// needs the chat endpoint, a key and a deployment that can see.
func (c *Client) CanTranscribe() bool {
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || !c.store.HasChatCredentials() {
		return false
	}
	_, ok := c.VisionDeployment(cfg.ChatModel)
	return ok
}

// Transcribe reads the text off a series of page images.
//
// Every page is a request of its own. Batching them would save overhead but
// makes the result worse: the model starts summarizing across pages instead of
// transcribing, and a long document runs into the context limit halfway
// through. One page per request also means a page the model cannot read costs
// that page and nothing else.
//
// prompt is the instruction handed to the model and pageLabel formats the
// header of each page; both come from the caller so the wording follows the UI
// language.
func (c *Client) Transcribe(ctx context.Context, prompt string, pageLabel func(page, total int) string, images []ImageContent) (TranscribeResult, error) {
	var result TranscribeResult
	if len(images) == 0 {
		return result, fmt.Errorf("nothing to transcribe")
	}

	cfg := c.store.Get()
	model, ok := c.VisionDeployment(cfg.ChatModel)
	if !ok {
		return result, fmt.Errorf("no vision capable deployment configured")
	}

	opts := ChatOptions{Model: model}
	if cfg.Foundry && cfg.EnabledDeployments != nil {
		opts.Model = ""
	}
	var sb strings.Builder

	for i, img := range images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		label := pageLabel(i+1, len(images))

		var page strings.Builder
		turn, err := c.streamTurn(ctx, opts, []Message{
			{Role: "system", Content: prompt},
			{Role: "user", Content: label, Images: []ImageContent{img}},
		}, nil, func(delta string) error {
			if len(delta) > (8<<20)-page.Len() || len(delta) > (8<<20)-sb.Len()-page.Len() {
				return fmt.Errorf("transcription exceeds the 8 MiB text limit")
			}
			page.WriteString(delta)
			return nil
		})
		if err != nil {
			return TranscribeResult{}, fmt.Errorf("transcription failed on page %d of %d; no partial document was saved: %w", i+1, len(images), err)
		}

		result.Usage.PromptTokens += turn.Usage.PromptTokens
		result.Usage.CompletionTokens += turn.Usage.CompletionTokens
		result.Usage.TotalTokens += turn.Usage.TotalTokens
		if turn.Model != "" {
			result.Model = turn.Model
		}

		text := strings.TrimSpace(page.String())
		if text == "" {
			continue // a blank page carries nothing
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(label)
		sb.WriteString("\n")
		sb.WriteString(text)
		result.Pages++
	}

	result.Text = sb.String()
	if result.Pages == 0 {
		return result, fmt.Errorf("the pages carry no readable text")
	}
	return result, nil
}
