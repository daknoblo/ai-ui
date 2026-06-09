// Package websearch bietet eine provider-agnostische Web-Suche, deren Ergebnisse
// als zusätzlicher Kontext in den Chat einfließen können.
package websearch

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
)

// Result ist ein einzelnes Suchergebnis.
type Result struct {
	Title   string
	URL     string
	Content string
}

// Provider abstrahiert einen konkreten Suchanbieter.
type Provider interface {
	// Search liefert bis zu maxResults Treffer zur Anfrage.
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
	// Name liefert den Anzeigenamen des Providers.
	Name() string
}

// Provider-Bezeichner für die Konfiguration.
const (
	ProviderNone    = ""
	ProviderTavily  = "tavily"
	ProviderBrave   = "brave"
	ProviderSearXNG = "searxng"
)

// Client wählt anhand der Konfiguration den passenden Provider und führt Suchen aus.
type Client struct {
	store *config.Store
	http  *http.Client
}

// New erzeugt einen Such-Client.
func New(store *config.Store) *Client {
	return &Client{
		store: store,
		http:  &http.Client{Timeout: 20 * time.Second},
	}
}

// Enabled gibt an, ob ein Suchanbieter konfiguriert ist.
func (c *Client) Enabled() bool {
	return c.provider() != nil
}

// provider baut den aktiven Provider aus der Konfiguration. Liefert nil, wenn
// keiner konfiguriert oder ein benötigter Key/Endpoint fehlt.
func (c *Client) provider() Provider {
	cfg := c.store.Get()
	key := c.store.SearchAPIKey()
	switch strings.ToLower(strings.TrimSpace(cfg.SearchProvider)) {
	case ProviderTavily:
		if key == "" {
			return nil
		}
		return &tavilyProvider{http: c.http, apiKey: key}
	case ProviderBrave:
		if key == "" {
			return nil
		}
		return &braveProvider{http: c.http, apiKey: key}
	case ProviderSearXNG:
		if strings.TrimSpace(cfg.SearchEndpoint) == "" {
			return nil
		}
		return &searxngProvider{http: c.http, endpoint: cfg.SearchEndpoint}
	default:
		return nil
	}
}

// Search führt eine Suche mit dem konfigurierten Provider aus.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	p := c.provider()
	if p == nil {
		return nil, fmt.Errorf("keine websuche konfiguriert")
	}
	max := c.store.Get().SearchMaxResults
	if max <= 0 {
		max = 5
	}
	return p.Search(ctx, query, max)
}

// Verify prüft, ob der konfigurierte Provider erreichbar ist und antwortet.
func (c *Client) Verify(ctx context.Context) error {
	p := c.provider()
	if p == nil {
		return fmt.Errorf("keine websuche konfiguriert")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := p.Search(ctx, "ping", 1); err != nil {
		return err
	}
	return nil
}

// ProviderName liefert den Namen des aktiven Providers (oder "").
func (c *Client) ProviderName() string {
	if p := c.provider(); p != nil {
		return p.Name()
	}
	return ""
}

// truncate kürzt einen Text auf höchstens n Runen.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
