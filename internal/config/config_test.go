package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/i18n"
)

// TestParseModelList checks the splitting of separated model lists including
// trimming as well as empty-field and duplicate filtering.
func TestParseModelList(t *testing.T) {
	got := ParseModelList(" gpt-4o, gpt-4o\n gpt-4o-mini \n\n, o3 ,")
	want := []string{"gpt-4o", "gpt-4o-mini", "o3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseModelList = %#v, want %#v", got, want)
	}
	if ParseModelList("   ") != nil {
		t.Fatalf("expected nil for empty input")
	}
}

// TestOverridesApplyAndLocks makes sure configured overrides layer over the
// effective configuration and produce the matching locks.
func TestOverridesApplyAndLocks(t *testing.T) {
	o := Overrides{
		Endpoint:            "https://env.example",
		ChatModels:          []string{"gpt-4o"},
		EmbeddingDeployment: "emb-env",
	}
	locks := o.locks()
	if !locks.Endpoint || !locks.EmbeddingDeployment {
		t.Fatalf("configured fields must be locked: %+v", locks)
	}
	if locks.ChatDeployment || locks.APIVersion || locks.EmbeddingEndpoint || locks.EmbeddingAPIVersion {
		t.Fatalf("unset fields must not be locked: %+v", locks)
	}
	if !locks.Any() {
		t.Fatalf("Any() must report true when fields are locked")
	}

	base := Config{Endpoint: "https://stored", ChatDeployment: "stored-dep", APIVersion: "v1"}
	eff := o.apply(base)
	if eff.Endpoint != "https://env.example" {
		t.Errorf("endpoint override was not applied: %q", eff.Endpoint)
	}
	if eff.ChatDeployment != "stored-dep" {
		t.Errorf("an unset override must not change the stored value: %q", eff.ChatDeployment)
	}
	if !reflect.DeepEqual(eff.ChatModels, []string{"gpt-4o"}) {
		t.Errorf("ChatModels override was not applied: %#v", eff.ChatModels)
	}
}

// TestModelListIsEnvOnly makes sure the model list never comes from the stored
// configuration and that a pinned model disappears with it.
func TestModelListIsEnvOnly(t *testing.T) {
	stored := Config{ChatModel: "gpt-4o", ChatModels: []string{"gpt-4o", "o3"}}
	if eff := (Overrides{}).apply(stored); eff.ChatModels != nil || eff.ChatModel != "" {
		t.Errorf("without AZURE_MODELS the list and the pinned model must be empty: %#v", eff)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, Keys{API: "key"}, Overrides{ChatModels: []string{"gpt-4o"}})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Save(s.Get()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "chat_models") {
		t.Errorf("the model list must not be persisted: %s", data)
	}
}

// TestStoreGetAppliesOverrides checks that Get() returns the effective
// configuration (stored values plus overrides).
func TestStoreGetAppliesOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, Keys{API: "key"}, Overrides{Endpoint: "https://env.example"})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.Get().Endpoint; got != "https://env.example" {
		t.Fatalf("Get().Endpoint = %q, expected the override", got)
	}
	if !s.Locks().Endpoint {
		t.Fatalf("endpoint must be locked")
	}
}

// TestSaveKeepsLockedFields makes sure that saving changes neither the
// effective configuration nor the raw file for locked fields, while unlocked
// fields are stored normally.
func TestSaveKeepsLockedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, Keys{API: "key"}, Overrides{Endpoint: "https://env.example"})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Try to overwrite the locked endpoint field and set an unlocked one at the
	// same time.
	cfg := s.Get()
	cfg.Endpoint = "https://attempted-overwrite"
	cfg.ChatDeployment = "gpt-4o"
	if err := s.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := s.Get().Endpoint; got != "https://env.example" {
		t.Errorf("locked endpoint was modified: %q", got)
	}
	if got := s.Get().ChatDeployment; got != "gpt-4o" {
		t.Errorf("unlocked field was not stored: %q", got)
	}

	// The raw file must not contain the ENV value.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw Config
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if raw.Endpoint != "" {
		t.Errorf("the raw configuration must not store the ENV/UI value, was %q", raw.Endpoint)
	}
	if raw.ChatDeployment != "gpt-4o" {
		t.Errorf("unlocked field missing from the raw configuration: %q", raw.ChatDeployment)
	}
}

// TestLanguageIsNormalized ensures a hand-edited or unknown language code never
// reaches the templates.
func TestLanguageIsNormalized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, Keys{API: "key"}, Overrides{})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.Language(); got != i18n.Default {
		t.Errorf("default language = %q, want %q", got, i18n.Default)
	}

	cfg := s.Get()
	cfg.Language = "klingon"
	if err := s.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := s.Language(); got != i18n.Default {
		t.Errorf("unknown language = %q, want the default %q", got, i18n.Default)
	}

	cfg.Language = "DE"
	if err := s.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := s.Language(); got != i18n.DE {
		t.Errorf("language = %q, want %q", got, i18n.DE)
	}
}

// TestSetChatModelUsesEnvList only allows pinning a model that AZURE_MODELS
// provides.
func TestSetChatModelUsesEnvList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, Keys{API: "key"}, Overrides{ChatModels: []string{"gpt-4o", "o3"}})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetChatModel("o3"); err != nil {
		t.Fatalf("SetChatModel(o3) should be allowed: %v", err)
	}
	if got := s.Get().ChatModel; got != "o3" {
		t.Errorf("ChatModel = %q, want o3", got)
	}
	if err := s.SetChatModel("unknown"); err == nil {
		t.Errorf("an unknown model must be rejected")
	}
}

// TestIsConfiguredWithOverrides confirms that endpoint values set via ENV count
// towards the "configured" detection.
func TestIsConfiguredWithOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path, Keys{API: "key"}, Overrides{
		Endpoint:       "https://env.example",
		ChatDeployment: "gpt-4o",
	})
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// APIVersion comes from the defaults, endpoint/deployment from ENV, key set.
	if !s.IsConfigured() {
		t.Fatalf("expected configured with ENV overrides")
	}

	// Without an API key it must not count as configured.
	s2 := NewStore(filepath.Join(t.TempDir(), "config.json"), Keys{}, Overrides{
		Endpoint:       "https://env.example",
		ChatDeployment: "gpt-4o",
	})
	if _, err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s2.IsConfigured() {
		t.Fatalf("must not be configured without an API key")
	}
}
