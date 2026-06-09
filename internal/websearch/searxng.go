package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// searxngProvider nutzt eine selbst gehostete SearXNG-Instanz (JSON-API).
// Es wird kein API-Key benötigt, nur die Basis-URL der Instanz.
type searxngProvider struct {
	http     *http.Client
	endpoint string
}

func (p *searxngProvider) Name() string { return "SearXNG" }

type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (p *searxngProvider) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	base := strings.TrimRight(p.endpoint, "/")
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readSearchError("searxng", resp)
	}

	var out searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	results := make([]Result, 0, maxResults)
	for _, r := range out.Results {
		if len(results) >= maxResults {
			break
		}
		results = append(results, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     r.URL,
			Content: truncate(strings.TrimSpace(r.Content), 1500),
		})
	}
	return results, nil
}
