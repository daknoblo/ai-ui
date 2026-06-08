package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config enthält die in der UI einstellbaren Werte. Der API-Key wird bewusst
// NICHT hier gespeichert, sondern ausschließlich zur Laufzeit aus der
// Umgebungsvariable AZURE_API_KEY bezogen.
type Config struct {
	Endpoint            string  `json:"endpoint"`             // z.B. https://my-router.openai.azure.com
	ChatDeployment      string  `json:"chat_deployment"`      // Deployment-Name des Chat-Modells
	APIVersion          string  `json:"api_version"`          // z.B. 2024-08-01-preview
	EmbeddingDeployment string  `json:"embedding_deployment"` // Deployment-Name des Embedding-Modells
	SystemPrompt        string  `json:"system_prompt"`
	Temperature         float64 `json:"temperature"`
}

// Defaults liefert sinnvolle Startwerte.
func Defaults() Config {
	return Config{
		Endpoint:            "",
		ChatDeployment:      "",
		APIVersion:          "2024-08-01-preview",
		EmbeddingDeployment: "",
		SystemPrompt:        "Du bist ein hilfreicher Assistent. Antworte präzise und nutze den bereitgestellten Kontext, wenn er relevant ist.",
		Temperature:         0.7,
	}
}

// Store verwaltet das Laden und Speichern der Konfiguration als JSON-Datei.
type Store struct {
	path   string
	apiKey string

	mu  sync.RWMutex
	cur Config
}

// NewStore erzeugt einen Konfigurationsspeicher für den angegebenen Pfad.
// apiKey stammt aus der Umgebung und wird niemals persistiert.
func NewStore(path, apiKey string) *Store {
	return &Store{path: path, apiKey: apiKey, cur: Defaults()}
}

// Load liest die Konfiguration von der Festplatte. Existiert keine Datei,
// werden Defaults verwendet und gespeichert.
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
	s.cur = cfg
	return s.cur, nil
}

// Get liefert eine Kopie der aktuellen Konfiguration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Save schreibt die Konfiguration atomar auf die Festplatte.
func (s *Store) Save(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeLocked(cfg); err != nil {
		return err
	}
	s.cur = cfg
	return nil
}

// APIKey liefert das aus der Umgebung geladene Secret.
func (s *Store) APIKey() string {
	return s.apiKey
}

// HasAPIKey gibt an, ob ein API-Key gesetzt wurde.
func (s *Store) HasAPIKey() bool {
	return s.apiKey != ""
}

// IsConfigured prüft, ob die Mindestangaben für Chat-Anfragen vorhanden sind.
func (s *Store) IsConfigured() bool {
	c := s.Get()
	return c.Endpoint != "" && c.ChatDeployment != "" && c.APIVersion != "" && s.apiKey != ""
}

func (s *Store) writeLocked(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
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
