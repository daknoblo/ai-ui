package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Config enthält die in der UI einstellbaren Werte. Der API-Key wird bewusst
// NICHT hier gespeichert, sondern ausschließlich zur Laufzeit aus der
// Umgebungsvariable AZURE_API_KEY bezogen.
type Config struct {
	Endpoint            string   `json:"endpoint"`              // Chat: z.B. https://my-router.openai.azure.com
	ChatDeployment      string   `json:"chat_deployment"`       // Deployment-Name des Chat-Modells (bzw. Routers)
	ChatModel           string   `json:"chat_model"`            // optional; erzwingt ein Modell statt Router-Auswahl
	ChatModels          []string `json:"chat_models"`           // auswählbare Modelle für das Header-Menü
	APIVersion          string   `json:"api_version"`           // z.B. 2024-08-01-preview
	EmbeddingEndpoint   string   `json:"embedding_endpoint"`    // optional; fällt auf Endpoint zurück
	EmbeddingDeployment string   `json:"embedding_deployment"`  // Deployment-Name des Embedding-Modells
	EmbeddingAPIVersion string   `json:"embedding_api_version"` // optional; fällt auf APIVersion zurück
	SearchProvider      string   `json:"search_provider"`       // "", "tavily", "brave", "searxng"
	SearchEndpoint      string   `json:"search_endpoint"`       // Basis-URL für SearXNG
	SearchMaxResults    int      `json:"search_max_results"`    // Anzahl Treffer (Default 5)
	SystemPrompt        string   `json:"system_prompt"`
	Temperature         float64  `json:"temperature"`
}

// EmbeddingVersion liefert die für Embeddings zu nutzende API-Version.
// Ist kein eigener Wert gesetzt, wird die allgemeine APIVersion verwendet.
func (c Config) EmbeddingVersion() string {
	if c.EmbeddingAPIVersion != "" {
		return c.EmbeddingAPIVersion
	}
	return c.APIVersion
}

// EmbeddingHost liefert den für Embeddings zu nutzenden Endpoint.
// Ist kein eigener Wert gesetzt, wird der Chat-Endpoint verwendet.
func (c Config) EmbeddingHost() string {
	if c.EmbeddingEndpoint != "" {
		return c.EmbeddingEndpoint
	}
	return c.Endpoint
}

// Defaults liefert sinnvolle Startwerte.
func Defaults() Config {
	return Config{
		Endpoint:            "",
		ChatDeployment:      "",
		ChatModel:           "",
		ChatModels:          nil,
		APIVersion:          "2024-08-01-preview",
		EmbeddingEndpoint:   "",
		EmbeddingDeployment: "",
		EmbeddingAPIVersion: "",
		SearchProvider:      "",
		SearchEndpoint:      "",
		SearchMaxResults:    5,
		SystemPrompt:        "Du bist ein hilfreicher Assistent. Antworte präzise und nutze den bereitgestellten Kontext, wenn er relevant ist.",
		Temperature:         0.7,
	}
}

// Store verwaltet das Laden und Speichern der Konfiguration als JSON-Datei.
type Store struct {
	path            string
	apiKey          string
	embeddingAPIKey string
	searchAPIKey    string

	mu  sync.RWMutex
	cur Config
}

// NewStore erzeugt einen Konfigurationsspeicher für den angegebenen Pfad.
// Die API-Keys stammen aus der Umgebung und werden niemals persistiert.
// Ist embeddingAPIKey leer, wird für Embeddings apiKey verwendet.
func NewStore(path, apiKey, embeddingAPIKey, searchAPIKey string) *Store {
	return &Store{
		path:            path,
		apiKey:          apiKey,
		embeddingAPIKey: embeddingAPIKey,
		searchAPIKey:    searchAPIKey,
		cur:             Defaults(),
	}
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

// SetChatModel ändert nur das aktiv erzwungene Modell und speichert die Konfig.
// Ein leerer Wert bedeutet "Router entscheidet". Werte außerhalb der gepflegten
// Liste werden abgelehnt, leere Eingabe ist immer erlaubt.
func (s *Store) SetChatModel(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if model != "" {
		allowed := false
		for _, m := range s.cur.ChatModels {
			if m == model {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("unbekanntes modell: %s", model)
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

// APIKey liefert das aus der Umgebung geladene Secret.
func (s *Store) APIKey() string {
	return s.apiKey
}

// HasAPIKey gibt an, ob ein API-Key gesetzt wurde.
func (s *Store) HasAPIKey() bool {
	return s.apiKey != ""
}

// EmbeddingAPIKey liefert den Key für Embeddings. Ist kein eigener Key gesetzt,
// wird der allgemeine API-Key zurückgegeben.
func (s *Store) EmbeddingAPIKey() string {
	if s.embeddingAPIKey != "" {
		return s.embeddingAPIKey
	}
	return s.apiKey
}

// HasEmbeddingAPIKey gibt an, ob ein (eigener oder geerbter) Key für Embeddings vorhanden ist.
func (s *Store) HasEmbeddingAPIKey() bool {
	return s.EmbeddingAPIKey() != ""
}

// HasOwnEmbeddingAPIKey gibt an, ob ein dedizierter Embedding-Key gesetzt wurde.
func (s *Store) HasOwnEmbeddingAPIKey() bool {
	return s.embeddingAPIKey != ""
}

// SearchAPIKey liefert den aus der Umgebung geladenen Such-API-Key.
func (s *Store) SearchAPIKey() string {
	return s.searchAPIKey
}

// HasSearchAPIKey gibt an, ob ein Such-API-Key gesetzt wurde.
func (s *Store) HasSearchAPIKey() bool {
	return s.searchAPIKey != ""
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
