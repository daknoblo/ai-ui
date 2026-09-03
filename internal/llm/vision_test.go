package llm

import "testing"

// TestSupportsVision covers the families the heuristic has to tell apart.
func TestSupportsVision(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"", true}, // the router picks a model that fits the request
		{"gpt-5.6-sol", true},
		{"gpt-4o", true},
		{"gpt-4o-mini", true},
		{"gpt-4.1", true},
		{"o3", true},
		{"prod-gpt-4o-eu", true},
		{"gpt-4", false},
		{"gpt-4-0613", false},
		{"gpt-3.5-turbo", false},
		{"gpt-35-turbo", false},
		{"o1-mini", false},
		{"o3-mini", false},
		{"text-embedding-3-large", false},
		{"gpt-image-2", false},
		{"dall-e-3", false},
	}
	for _, tc := range cases {
		if got := SupportsVision(tc.model); got != tc.want {
			t.Errorf("SupportsVision(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestVisionModel checks the automatic switch: a model that cannot see is
// replaced by the first candidate that can, and a capable choice is kept.
func TestVisionModel(t *testing.T) {
	cases := []struct {
		name       string
		chosen     string
		candidates []string
		want       string
		wantOK     bool
	}{
		{
			name:       "a capable model is kept",
			chosen:     "gpt-4o",
			candidates: []string{"gpt-3.5-turbo", "gpt-5.1"},
			want:       "gpt-4o",
			wantOK:     true,
		},
		{
			name:       "a blind model is replaced",
			chosen:     "gpt-3.5-turbo",
			candidates: []string{"o1-mini", "gpt-3.5-turbo", "gpt-5.1", "gpt-4o"},
			want:       "gpt-5.1",
			wantOK:     true,
		},
		{
			name:       "the router is left alone",
			chosen:     "",
			candidates: []string{"gpt-3.5-turbo"},
			want:       "",
			wantOK:     true,
		},
		{
			name:       "no candidate qualifies",
			chosen:     "gpt-3.5-turbo",
			candidates: []string{"o1-mini", "gpt-35-turbo"},
			want:       "",
		},
		{
			name:       "no candidates configured",
			chosen:     "gpt-3.5-turbo",
			candidates: nil,
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := VisionModel(tc.chosen, tc.candidates)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("VisionModel(%q, %v) = %q, %v; want %q, %v",
					tc.chosen, tc.candidates, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
