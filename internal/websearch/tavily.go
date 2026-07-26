package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// tavilyProvider uses the Tavily Search API, which returns already extracted
// page content and is therefore particularly well suited for RAG.
type tavilyProvider struct {
	http   *http.Client
	apiKey string
}

func (p *tavilyProvider) Name() string { return "Tavily" }

type tavilyRequest struct {
	Query       string `json:"query"`
	MaxResults  int    `json:"max_results"`
	SearchDepth string `json:"search_depth"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (p *tavilyProvider) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	body, err := json.Marshal(tavilyRequest{
		Query:       query,
		MaxResults:  maxResults,
		SearchDepth: "basic",
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, readSearchError("tavily", resp)
	}

	var out tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     r.URL,
			Content: truncate(strings.TrimSpace(r.Content), maxContentRunes),
		})
	}
	return results, nil
}

// readSearchError reads and formats an error response from a search provider.
func readSearchError(provider string, resp *http.Response) error {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(buf.String())
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("%s error %d: %s", provider, resp.StatusCode, msg)
}
