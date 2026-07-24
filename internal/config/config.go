package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	SearchAuto          bool     `json:"search_auto"`           // Modell darf Websuche selbst per Tool-Calling auslösen
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

// Overrides bündelt die per Umgebungsvariable festgelegten Endpoint-Werte. Jedes
// nicht-leere Feld hat Vorrang vor der gespeicherten Konfiguration und wird in
// der UI nur schreibgeschützt angezeigt (das Eingabefeld ist deaktiviert).
type Overrides struct {
	Endpoint            string   // AZURE_ENDPOINT
	ChatDeployment      string   // AZURE_CHAT_DEPLOYMENT
	ChatModels          []string // AZURE_CHAT_MODELS (Komma- oder Zeilen-getrennt)
	APIVersion          string   // AZURE_API_VERSION
	EmbeddingEndpoint   string   // AZURE_EMBEDDING_ENDPOINT
	EmbeddingDeployment string   // AZURE_EMBEDDING_DEPLOYMENT
	EmbeddingAPIVersion string   // AZURE_EMBEDDING_API_VERSION
}

// apply legt die gesetzten Overrides über die übergebene Konfiguration und
// liefert die effektive Konfiguration zurück.
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

// locks leitet aus den gesetzten Overrides ab, welche Felder gesperrt sind.
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

// Locks gibt an, welche Endpoint-Felder per Umgebungsvariable festgelegt (und
// damit in der UI gesperrt) sind. Die Felder entsprechen den Overrides.
type Locks struct {
	Endpoint            bool
	ChatDeployment      bool
	ChatModels          bool
	APIVersion          bool
	EmbeddingEndpoint   bool
	EmbeddingDeployment bool
	EmbeddingAPIVersion bool
}

// Any gibt an, ob mindestens ein Endpoint-Feld per Umgebungsvariable gesperrt ist.
func (l Locks) Any() bool {
	return l.Endpoint || l.ChatDeployment || l.ChatModels || l.APIVersion ||
		l.EmbeddingEndpoint || l.EmbeddingDeployment || l.EmbeddingAPIVersion
}

// ParseModelList zerlegt eine durch Zeilen oder Kommas getrennte Liste in
// bereinigte, eindeutige Modellnamen (z.B. für AZURE_CHAT_MODELS).
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
		SearchAuto:          false,
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
	overrides       Overrides // per Umgebungsvariable festgelegte Endpoint-Werte
	locks           Locks     // abgeleitet aus overrides; welche Felder gesperrt sind

	mu  sync.RWMutex
	cur Config // gespeicherte Rohkonfiguration (ohne angewandte Overrides)
}

// NewStore erzeugt einen Konfigurationsspeicher für den angegebenen Pfad.
// Die API-Keys stammen aus der Umgebung und werden niemals persistiert.
// Ist embeddingAPIKey leer, wird für Embeddings apiKey verwendet. Gesetzte
// overrides überschreiben die gespeicherten Endpoint-Werte und sperren die
// zugehörigen Felder in der UI.
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

// Get liefert die effektive Konfiguration: die gespeicherten Werte mit den
// per Umgebungsvariable gesetzten Overrides überlagert.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.overrides.apply(s.cur)
}

// Locks liefert die Information, welche Endpoint-Felder per Umgebungsvariable
// festgelegt und damit in der UI gesperrt sind.
func (s *Store) Locks() Locks {
	return s.locks
}

// Save schreibt die Konfiguration atomar auf die Festplatte. Per Umgebungsvariable
// gesperrte Felder behalten dabei ihren gespeicherten Rohwert und können nicht
// über die UI verändert werden.
func (s *Store) Save(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keepLockedLocked(&cfg)
	if err := s.writeLocked(cfg); err != nil {
		return err
	}
	s.cur = cfg
	return nil
}

// keepLockedLocked stellt sicher, dass gesperrte (per ENV gesetzte) Felder ihren
// bereits gespeicherten Rohwert behalten und nicht durch UI-Eingaben oder den
// Override-Wert überschrieben werden. Aufrufer müssen s.mu halten.
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

// SetChatModel ändert nur das aktiv erzwungene Modell und speichert die Konfig.
// Ein leerer Wert bedeutet "Router entscheidet". Werte außerhalb der gepflegten
// Liste werden abgelehnt, leere Eingabe ist immer erlaubt.
func (s *Store) SetChatModel(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if model != "" {
		// Gegen die effektive Modell-Liste prüfen (inkl. ENV-Override).
		eff := s.overrides.apply(s.cur)
		allowed := false
		for _, m := range eff.ChatModels {
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
