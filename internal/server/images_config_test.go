package server

import (
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// TestCrossResourceImageKey covers the setup that answers 401: the image
// endpoint points at another resource while the chat key is inherited.
func TestCrossResourceImageKey(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		image    string
		ownKey   bool
		want     bool
	}{
		{"other resource without own key", "https://a.services.ai.azure.com/openai/v1", "https://b.openai.azure.com/openai/v1", false, true},
		{"other resource with own key", "https://a.services.ai.azure.com/openai/v1", "https://b.openai.azure.com/openai/v1", true, false},
		{"same host", "https://a.services.ai.azure.com/openai/v1", "https://a.services.ai.azure.com/openai/v1", false, false},
		{"inherited endpoint", "https://a.services.ai.azure.com/openai/v1", "", false, false},
	}
	for _, tc := range tests {
		cfg := config.Config{Endpoint: tc.endpoint, ImageEndpoint: tc.image}
		if got := crossResourceImageKey(cfg, tc.ownKey); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPickerFollowsMode makes sure the header offers the image deployments
// while the chat is in image mode and the chat models otherwise.
func TestPickerFollowsMode(t *testing.T) {
	cfg := config.Config{
		ChatModels:      []string{"model-router", "gpt-5.1"},
		ImageModels:     []string{"gpt-image-2"},
		ImageDeployment: "gpt-image-2",
	}

	chat := storage.Chat{ID: 7, Model: "gpt-5.1", Mode: storage.ChatModeChat}
	picker := pickerFor(chat.ID, cfg, &chat)
	if picker.Current != "gpt-5.1" || len(picker.Models) != 2 || !picker.AllowAuto {
		t.Errorf("chat mode picker = %+v", picker)
	}

	chat.Mode = storage.ChatModeImage
	picker = pickerFor(chat.ID, cfg, &chat)
	if picker.Current != "gpt-image-2" || len(picker.Models) != 1 || picker.AllowAuto {
		t.Errorf("image mode picker = %+v", picker)
	}

	// A chat without its own image model falls back to the configured one.
	chat.ImageModel = "MAI-Image-2.5"
	if got := imageModelOf(cfg, &chat); got != "MAI-Image-2.5" {
		t.Errorf("image model of the chat = %q", got)
	}
	chat.ImageModel = ""
	if got := imageModelOf(cfg, &chat); got != "gpt-image-2" {
		t.Errorf("fallback image model = %q", got)
	}
}
