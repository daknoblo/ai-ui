package llm

import (
	"slices"
	"testing"
)

// TestReasoningEfforts pins the model families down to the effort values the
// endpoint documents for them.
func TestReasoningEfforts(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		{"", effortsAny},                     // router: the answering model is unknown
		{"model-router", effortsAny},         // unknown name: offer everything
		{"claude-opus-4-7", effortsAny},      //
		{"gpt-5", effortsGPT5},               //
		{"gpt-5-mini", effortsGPT5},          //
		{"gpt-5.1", effortsGPT51},            //
		{"gpt-5.6-sol", effortsGPT51},        //
		{"gpt-5.1-chat", nil},                // chat tuned: answers without reasoning
		{"gpt-chat-latest", nil},             //
		{"grok-4.3", effortsAny},             //
		{"o3", effortsO},                     //
		{"prod-o4-mini", effortsO},           //
		{"o1-mini", nil},                     // the o-series model without effort
		{"gpt-4.1", nil},                     //
		{"gpt-4o", nil},                      //
		{"text-embedding-3-large", nil},      //
		{"gpt-image-2", nil},                 //
		{"GPT-5.1", effortsGPT51},            // case insensitive
		{"  gpt-5.1  ", effortsGPT51},        //
		{"my-orion-deployment", effortsAny},  // "o" not followed by a version digit
		{"claude-sonnet-4-5", effortsAny},    //
		{"deepseek-r1", effortsAny},          //
		{"gpt-35-turbo", nil},                //
		{"whisper", nil},                     //
		{"o1", effortsO},                     //
		{"gpt-5-nano", effortsGPT5},          //
		{"gpt-5.1-codex", effortsGPT51},      //
		{"llama-4-maverick", effortsAny},     //
		{"phi-4-reasoning", effortsAny},      //
		{"mistral-large", effortsAny},        //
		{"grok-4", effortsAny},               //
		{"dall-e-3", nil},                    //
		{"sora", nil},                        //
		{"model-router-prod", effortsAny},    //
		{"o4-mini-high", effortsO},           //
		{"gpt-4.1-mini", nil},                //
		{"my-gpt-5-deployment", effortsGPT5}, //
	}
	for _, tc := range tests {
		if got := ReasoningEfforts(tc.model); !slices.Equal(got, tc.want) {
			t.Errorf("ReasoningEfforts(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestNormalizeReasoningEffort keeps a chat within what its model offers.
func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		model, effort, want string
	}{
		{"gpt-5.1", "high", "high"},
		{"gpt-5.1", "HIGH", "high"},
		{"gpt-5.1", "minimal", ReasoningAuto}, // 5.1 replaced minimal with none
		{"gpt-5", "minimal", "minimal"},
		{"o3", "none", ReasoningAuto}, // the o-series has no "none"
		{"gpt-4.1", "high", ReasoningAuto},
		{"gpt-5.1", "", ReasoningAuto},
	}
	for _, tc := range tests {
		if got := NormalizeReasoningEffort(tc.model, tc.effort); got != tc.want {
			t.Errorf("NormalizeReasoningEffort(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
		}
	}
}
