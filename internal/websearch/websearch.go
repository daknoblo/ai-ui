// Package websearch provides a provider-agnostic web search whose results can
// be fed into the chat as additional context.
package websearch

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
)

// Result is a single search hit.
type Result struct {
	Title   string
	URL     string
	Content string
}

// Provider abstracts a concrete search backend.
type Provider interface {
	// Search returns up to maxResults hits for the query.
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
	// Name returns the display name of the provider.
	Name() string
}

// Provider identifiers used in the configuration.
const (
	ProviderNone    = ""
	ProviderTavily  = "tavily"
	ProviderBrave   = "brave"
	ProviderSearXNG = "searxng"
)

// maxContentRunes caps how much text of a single hit is passed to the model.
const maxContentRunes = 1500

// Client picks the provider matching the configuration and runs searches.
type Client struct {
	store *config.Store
	http  *http.Client
}

// New creates a search client. It uses a hardened transport because the SearXNG
// base URL is user supplied (see newSafeTransport).
func New(store *config.Store) *Client {
	return &Client{
		store: store,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: newSafeTransport(),
		},
	}
}

// Enabled reports whether a search provider is configured.
func (c *Client) Enabled() bool {
	return c.provider() != nil
}

// provider builds the active provider from the configuration. It returns nil if
// none is configured or a required key/endpoint is missing or invalid.
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
		endpoint := strings.TrimSpace(cfg.SearchEndpoint)
		if endpoint == "" || ValidateEndpoint(endpoint) != nil {
			return nil
		}
		return &searxngProvider{http: c.http, endpoint: endpoint}
	default:
		return nil
	}
}

// Search runs a query with the configured provider.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	p := c.provider()
	if p == nil {
		return nil, fmt.Errorf("no web search configured")
	}
	max := c.store.Get().SearchMaxResults
	if max <= 0 {
		max = 5
	}
	return p.Search(ctx, query, max)
}

// Verify checks whether the configured provider is reachable and responds.
func (c *Client) Verify(ctx context.Context) error {
	p := c.provider()
	if p == nil {
		return fmt.Errorf("no web search configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := p.Search(ctx, "ping", 1); err != nil {
		return err
	}
	return nil
}

// ProviderName returns the name of the active provider (or "").
func (c *Client) ProviderName() string {
	if p := c.provider(); p != nil {
		return p.Name()
	}
	return ""
}

// truncate shortens a text to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
