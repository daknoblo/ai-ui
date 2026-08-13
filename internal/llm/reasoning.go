package llm

import (
	"regexp"
	"strings"
)

// ReasoningAuto leaves the reasoning effort to the model: the parameter is not
// sent at all.
const ReasoningAuto = "auto"

// Effort values per model family. Which of them a model accepts differs, so the
// lists follow the documented support: the o-series knows low/medium/high, GPT-5
// added minimal, and GPT-5.1 replaced minimal with none and added xhigh.
var (
	effortsGPT51 = []string{ReasoningAuto, "none", "low", "medium", "high", "xhigh"}
	effortsGPT5  = []string{ReasoningAuto, "minimal", "low", "medium", "high"}
	effortsO     = []string{ReasoningAuto, "low", "medium", "high"}
	effortsAny   = []string{ReasoningAuto, "none", "minimal", "low", "medium", "high", "xhigh"}
)

// oSeries matches the reasoning models named o1, o3, o4 … including deployment
// names that carry a prefix such as "prod-o3-mini".
var oSeries = regexp.MustCompile(`(^|[^a-z0-9])o[1-9]`)

// ReasoningEfforts returns the reasoning effort values that may be offered for a
// model, cheapest first. An empty result means the model has no reasoning effort
// to set.
//
// The argument is a deployment name, so the decision is a heuristic on the model
// family; an empty name (the router decides) offers everything. Guessing too
// generously is harmless: an effort a model rejects is dropped and the request
// repeated (see chatRequest.dropRejected).
func ReasoningEfforts(model string) []string {
	name := strings.ToLower(strings.TrimSpace(model))
	switch {
	case name == "":
		return effortsAny
	case strings.Contains(name, "o1-mini"):
		return nil // the only o-series model without reasoning effort
	case containsAny(name, "gpt-image", "dall-e", "embedding", "whisper", "tts", "sora"):
		return nil
	case containsAny(name, "gpt-3.5", "gpt-35", "gpt-4"):
		return nil
	case strings.Contains(name, "gpt-5."):
		return effortsGPT51
	case strings.Contains(name, "gpt-5"):
		return effortsGPT5
	case oSeries.MatchString(name):
		return effortsO
	default:
		return effortsAny
	}
}

// SupportsReasoning reports whether an effort can be chosen for a model.
func SupportsReasoning(model string) bool {
	return len(ReasoningEfforts(model)) > 0
}

// NormalizeReasoningEffort keeps an effort within what the model offers and
// falls back to "auto" for everything else.
func NormalizeReasoningEffort(model, effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, allowed := range ReasoningEfforts(model) {
		if effort == allowed {
			return effort
		}
	}
	return ReasoningAuto
}

// containsAny reports whether s contains one of the substrings.
func containsAny(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
