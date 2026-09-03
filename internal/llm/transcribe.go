package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// TranscribeResult is the text read off a set of page images plus what it cost.
type TranscribeResult struct {
	Text  string
	Pages int // pages that produced text
	Model string
	Usage Usage
}

// maxConsecutiveFailures stops a transcription that is failing for a reason
// outside the individual page - an endpoint that is down, out of quota or
// rejecting the model. Every attempt after that costs without any prospect of
// succeeding.
const maxConsecutiveFailures = 3

// CanTranscribe reports whether reading images as text is possible at all: it
// needs the chat endpoint, a key and a deployment that can see.
func (c *Client) CanTranscribe() bool {
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || !c.store.HasAPIKey() {
		return false
	}
	_, ok := VisionModel(cfg.ChatModel, cfg.ChatModels)
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
	model, ok := VisionModel(cfg.ChatModel, cfg.ChatModels)
	if !ok {
		return result, fmt.Errorf("no vision capable deployment configured")
	}

	opts := ChatOptions{Model: model}
	var (
		sb          strings.Builder
		lastErr     error
		failures    int
		consecutive int
	)

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
			page.WriteString(delta)
			return nil
		})
		if err != nil {
			// One unreadable page must not throw away the rest of the document.
			slog.Warn("transcribe page", "page", i+1, "of", len(images), "err", err)
			lastErr = err
			failures++
			consecutive++
			// An endpoint that is down, rate limited or refusing the model
			// fails every page the same way. Retrying the whole document
			// against it only burns quota, so give up once it is clearly not a
			// problem of the individual page.
			if consecutive >= maxConsecutiveFailures {
				slog.Warn("giving up on the transcription",
					"consecutive_failures", consecutive, "read", result.Pages, "of", len(images))
				break
			}
			continue
		}
		consecutive = 0

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
		if lastErr != nil {
			return result, fmt.Errorf("transcription failed: %w", lastErr)
		}
		return result, fmt.Errorf("the pages carry no readable text")
	}
	if failures > 0 {
		slog.Warn("some pages could not be transcribed",
			"failed", failures, "read", result.Pages, "total", len(images))
	}
	return result, nil
}
