package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// braveProvider nutzt die Brave-Search-API (REST, Header-Auth).
type braveProvider struct {
	http   *http.Client
	apiKey string
}

func (p *braveProvider) Name() string { return "Brave Search" }

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (p *braveProvider) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(maxResults))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.search.brave.com/res/v1/web/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readSearchError("brave", resp)
	}

	var out braveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		results = append(results, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     r.URL,
			Content: truncate(stripHTML(r.Description), 1500),
		})
	}
	return results, nil
}

// stripHTML entfernt einfache HTML-Tags (Brave hebt Treffer mit <strong> hervor).
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
