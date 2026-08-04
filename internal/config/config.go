// Package config loads and persists the user-editable settings.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/daknoblo/ai-ui/internal/i18n"
)

// Config holds the values that can be changed in the UI. API keys are
// deliberately NOT stored here; they are read at runtime from environment
// variables only.
type Config struct {
	Language            string   `json:"language"`              // UI language: "en" or "de"
	Endpoint            string   `json:"endpoint"`              // chat, e.g. https://my-router.openai.azure.com
	ChatDeployment      string   `json:"chat_deployment"`       // deployment name of the chat model (or router)
	ChatModel           string   `json:"chat_model"`            // optional; pins a model instead of letting the router choose
	ChatModels          []string `json:"chat_models"`           // models offered in the header menu
	APIVersion          string   `json:"api_version"`           // e.g. 2024-08-01-preview
	EmbeddingEndpoint   string   `json:"embedding_endpoint"`    // optional; falls back to Endpoint
	EmbeddingDeployment string   `json:"embedding_deployment"`  // deployment name of the embedding model
	EmbeddingAPIVersion string   `json:"embedding_api_version"` // optional; falls back to APIVersion
	SearchProvider      string   `json:"search_provider"`       // "", "tavily", "brave", "searxng"
	SearchEndpoint      string   `json:"search_endpoint"`       // base URL of the SearXNG instance
	SearchMaxResults    int      `json:"search_max_results"`    // number of results (default 5)
	SearchAuto          bool     `json:"search_auto"`           // allow the model to trigger a web search via tool calling
	SystemPrompt        string   `json:"system_prompt"`         // empty means "use the localized default"
	Temperature         float64  `json:"temperature"`
}

// EmbeddingVersion returns the API version to use for embeddings. When no
// dedicated value is set the general APIVersion applies.
func (c Config) EmbeddingVersion() string {
	if c.EmbeddingAPIVersion != "" {
		return c.EmbeddingAPIVersion
	}
	return c.APIVersion
}

// EmbeddingHost returns the endpoint to use for embeddings. When no dedicated
// value is set the chat endpoint applies.
func (c Config) EmbeddingHost() string {
	if c.EmbeddingEndpoint != "" {
		return c.EmbeddingEndpoint
	}
	return c.Endpoint
}

// Overrides bundles the endpoint values pinned via environment variables. Every
// non-empty field takes precedence over the stored configuration and is shown
// read-only in the UI (the input field is disabled).
type Overrides struct {
	Endpoint            string   // AZURE_ENDPOINT
	ChatDeployment      string   // AZURE_DEPLOYMENT
	ChatModels          []string // AZURE_MODELS (comma or newline separated)
	APIVersion          string   // AZURE_API_VERSION
	EmbeddingEndpoint   string   // AZURE_EMBEDDING_ENDPOINT
	EmbeddingDeployment string   // AZURE_EMBEDDING_DEPLOYMENT
	EmbeddingAPIVersion string   // AZURE_EMBEDDING_API_VERSION
}

// apply layers the configured overrides on top of c and returns the effective
// configuration.
func (o Overrides) apply(c Config) Config {
	if o.Endpoint != "" {
		c.Endpoint = o.Endpoint
	}
	if o.ChatDeployment != "" {
		c.ChatDeployment = o.ChatDeployment
	}
	if len(o.ChatModels) > 0 {
		c.ChatModels = o.ChatModels
	}
	if o.APIVersion != "" {
		c.APIVersion = o.APIVersion
	}
	if o.EmbeddingEndpoint != "" {
		c.EmbeddingEndpoint = o.EmbeddingEndpoint
	}
	if o.EmbeddingDeployment != "" {
		c.EmbeddingDeployment = o.EmbeddingDeployment
	}
	if o.EmbeddingAPIVersion != "" {
		c.EmbeddingAPIVersion = o.EmbeddingAPIVersion
	}
	return c
}

// locks derives from the configured overrides which fields are read-only.
func (o Overrides) locks() Locks {
	return Locks{
		Endpoint:            o.Endpoint != "",
		ChatDeployment:      o.ChatDeployment != "",
		ChatModels:          len(o.ChatModels) > 0,
		APIVersion:          o.APIVersion != "",
		EmbeddingEndpoint:   o.EmbeddingEndpoint != "",
		EmbeddingDeployment: o.EmbeddingDeployment != "",
		EmbeddingAPIVersion: o.EmbeddingAPIVersion != "",
	}
}

// Locks reports which endpoint fields are pinned via environment variables and
// therefore locked in the UI. The fields mirror Overrides.
type Locks struct {
	Endpoint            bool
	ChatDeployment      bool
	ChatModels          bool
	APIVersion          bool
	EmbeddingEndpoint   bool
	EmbeddingDeployment bool
	EmbeddingAPIVersion bool
}

// Any reports whether at least one endpoint field is locked via the environment.
func (l Locks) Any() bool {
	return l.Endpoint || l.ChatDeployment || l.ChatModels || l.APIVersion ||
		l.EmbeddingEndpoint || l.EmbeddingDeployment || l.EmbeddingAPIVersion
}

// ParseModelList splits a newline or comma separated list into trimmed, unique
// model names (used for AZURE_MODELS, among others).
func ParseModelList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	seen := make(map[string]struct{}, len(fields))
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// defaultChatModels are the models the model router offers; they cannot be
// discovered from the endpoint, so they are curated here.
var defaultChatModels = []string{
	"gpt-5.5",
	"claude-opus-4-7",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
	"gpt-oss-120b",
	"grok-4-1-fast-reasoning",
}

// Defaults returns sensible initial values. SystemPrompt stays empty on purpose
// so that the localized default prompt is used and follows the UI language.
func Defaults() Config {
	return Config{
		Language:            i18n.Default,
		Endpoint:            "",
		ChatDeployment:      "",
		ChatModel:           "",
		ChatModels:          slices.Clone(defaultChatModels),
		APIVersion:          "2024-08-01-preview",
		EmbeddingEndpoint:   "",
		EmbeddingDeployment: "",
		EmbeddingAPIVersion: "",
		SearchProvider:      "",
		SearchEndpoint:      "",
		SearchMaxResults:    5,
		SearchAuto:          false,
		SystemPrompt:        "",
		Temperature:         0.7,
	}
}

// Store manages loading and saving the configuration as a JSON file.
type Store struct {
	path            string
	apiKey          string
	embeddingAPIKey string
	searchAPIKey    string
	overrides       Overrides // endpoint values pinned via environment variables
	locks           Locks     // derived from overrides: which fields are read-only

	mu  sync.RWMutex
	cur Config // stored raw configuration (without overrides applied)
}

// NewStore creates a configuration store for the given path. The API keys come
// from the environment and are never persisted. An empty embeddingAPIKey makes
// embeddings reuse apiKey. Configured overrides replace the stored endpoint
// values and lock the corresponding fields in the UI.
func NewStore(path, apiKey, embeddingAPIKey, searchAPIKey string, overrides Overrides) *Store {
	return &Store{
		path:            path,
		apiKey:          apiKey,
		embeddingAPIKey: embeddingAPIKey,
		searchAPIKey:    searchAPIKey,
		overrides:       overrides,
		locks:           overrides.locks(),
		cur:             Defaults(),
	}
}

// Load reads the configuration from disk. When no file exists the defaults are
// used and written out.
func (s *Store) Load() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.cur = Defaults()
		if werr := s.writeLocked(s.cur); werr != nil {
			return s.cur, werr
		}
		return s.cur, nil
	}
	if err != nil {
		return s.cur, err
	}

	cfg := Defaults()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return s.cur, err
	}
	cfg.Language = i18n.Normalize(cfg.Language)
	s.cur = cfg
	return s.cur, nil
}

// Get returns the effective configuration: the stored values with the
// environment overrides layered on top.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.overrides.apply(s.cur)
}

// Language returns the normalized UI language. It is called on every template
// render, so it deliberately avoids copying the whole configuration.
func (s *Store) Language() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return i18n.Normalize(s.cur.Language)
}

// Locks reports which endpoint fields are pinned via environment variables and
// therefore locked in the UI.
func (s *Store) Locks() Locks {
	return s.locks
}

// Save writes the configuration to disk atomically. Fields locked via the
// environment keep their stored raw value and cannot be changed through the UI.
func (s *Store) Save(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.Language = i18n.Normalize(cfg.Language)
	s.keepLockedLocked(&cfg)
	if err := s.writeLocked(cfg); err != nil {
		return err
	}
	s.cur = cfg
	return nil
}

// keepLockedLocked makes sure that locked (environment-pinned) fields keep the
// raw value already on disk and are not overwritten by UI input or by the
// override value itself. Callers must hold s.mu.
func (s *Store) keepLockedLocked(cfg *Config) {
	if s.locks.Endpoint {
		cfg.Endpoint = s.cur.Endpoint
	}
	if s.locks.ChatDeployment {
		cfg.ChatDeployment = s.cur.ChatDeployment
	}
	if s.locks.ChatModels {
		cfg.ChatModels = s.cur.ChatModels
	}
	if s.locks.APIVersion {
		cfg.APIVersion = s.cur.APIVersion
	}
	if s.locks.EmbeddingEndpoint {
		cfg.EmbeddingEndpoint = s.cur.EmbeddingEndpoint
	}
	if s.locks.EmbeddingDeployment {
		cfg.EmbeddingDeployment = s.cur.EmbeddingDeployment
	}
	if s.locks.EmbeddingAPIVersion {
		cfg.EmbeddingAPIVersion = s.cur.EmbeddingAPIVersion
	}
}

// SetChatModel changes only the pinned model and persists the configuration.
// An empty value means "let the router decide". Values outside the curated list
// are rejected; empty input is always allowed.
func (s *Store) SetChatModel(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if model != "" {
		// Validate against the effective model list (including the ENV override).
		eff := s.overrides.apply(s.cur)
		allowed := false
		for _, m := range eff.ChatModels {
			if m == model {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("unknown model: %s", model)
		}
	}

	cfg := s.cur
	cfg.ChatModel = model
	if err := s.writeLocked(cfg); err != nil {
		return err
	}
	s.cur = cfg
	return nil
}

// APIKey returns the secret loaded from the environment.
func (s *Store) APIKey() string {
	return s.apiKey
}

// HasAPIKey reports whether an API key was provided.
func (s *Store) HasAPIKey() bool {
	return s.apiKey != ""
}

// EmbeddingAPIKey returns the key used for embeddings. When no dedicated key is
// set the general API key is returned.
func (s *Store) EmbeddingAPIKey() string {
	if s.embeddingAPIKey != "" {
		return s.embeddingAPIKey
	}
	return s.apiKey
}

// HasEmbeddingAPIKey reports whether a (dedicated or inherited) embedding key exists.
func (s *Store) HasEmbeddingAPIKey() bool {
	return s.EmbeddingAPIKey() != ""
}

// HasOwnEmbeddingAPIKey reports whether a dedicated embedding key was provided.
func (s *Store) HasOwnEmbeddingAPIKey() bool {
	return s.embeddingAPIKey != ""
}

// SearchAPIKey returns the web search API key loaded from the environment.
func (s *Store) SearchAPIKey() string {
	return s.searchAPIKey
}

// HasSearchAPIKey reports whether a web search API key was provided.
func (s *Store) HasSearchAPIKey() bool {
	return s.searchAPIKey != ""
}

// IsConfigured checks whether the minimum settings for chat requests are present.
func (s *Store) IsConfigured() bool {
	c := s.Get()
	return c.Endpoint != "" && c.ChatDeployment != "" && c.APIVersion != "" && s.apiKey != ""
}

// writeLocked serializes cfg and replaces the config file atomically. Callers
// must hold s.mu. The file is written with 0600 because it may contain
// endpoint details that are not meant to be world-readable.
func (s *Store) writeLocked(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
