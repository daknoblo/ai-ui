package llm

import "strings"

// blindModels are the chat models that accept text only. Everything else is
// assumed to understand images, because that is the default for the current
// generations and a wrong guess in this direction is corrected by the endpoint
// rather than by silently dropping an attachment.
//
// The argument is a deployment name, so the decision is a heuristic on the
// model family - the same approach ReasoningEfforts takes.
var blindModels = []string{
	"gpt-3.5", "gpt-35", // the 3.5 family predates vision entirely
	"o1-mini", "o3-mini", // the small o-series siblings are text only
	"embedding", "whisper", "tts", "moderation",
}

// SupportsVision reports whether a model can be given images. An empty name
// leaves the choice to the router, which routes a request with an attachment to
// a model that can read it.
func SupportsVision(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return true
	}
	// The image models generate pictures, they do not read them in a chat.
	if containsAny(name, "gpt-image", "dall-e", "sora") {
		return false
	}
	// "gpt-4" alone was text only; vision arrived with the turbo and o variants.
	if name == "gpt-4" || strings.HasPrefix(name, "gpt-4-0") || strings.HasPrefix(name, "gpt-4-3") {
		return false
	}
	return !containsAny(name, blindModels...)
}

// VisionModel returns the model to use for a request that carries images, and
// whether a usable one exists at all.
//
// The chosen model is preferred whenever it can see. Otherwise the first
// candidate that can is used, so attaching a picture does not force the user to
// change the model picker first. An empty name with ok is the router, which
// routes the request to a model that fits it.
func VisionModel(chosen string, candidates []string) (model string, ok bool) {
	if SupportsVision(chosen) {
		return chosen, true
	}
	for _, candidate := range candidates {
		if candidate != "" && SupportsVision(candidate) {
			return candidate, true
		}
	}
	return "", false
}
