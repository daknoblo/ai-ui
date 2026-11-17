// Package documentai provides bounded Azure Document Intelligence Layout analysis.
package documentai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	apiVersion = "2024-11-30"
	modelPath  = "/documentintelligence/documentModels/prebuilt-layout"
	MaxPages   = 100
	MaxInput   = 25 << 20
	maxOutput  = 8 << 20
)

// Page contains a page's extracted Markdown, including table structure.
type Page struct {
	Number int
	Text   string
}

type Client struct {
	endpoint   *url.URL
	key        string
	credential azcore.TokenCredential
	http       *http.Client
	gate       chan struct{}
	pollDelay  time.Duration
}

// New accepts only an explicit Azure public-cloud resource endpoint. Credentials
// are never forwarded to redirects or arbitrary operation URLs.
func New(endpoint, key string, credential azcore.TokenCredential) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || u.Port() != "" || (u.Path != "" && u.Path != "/") ||
		!strings.HasSuffix(u.Hostname(), ".cognitiveservices.azure.com") {
		return nil, errors.New("document analysis requires an HTTPS Azure Cognitive Services resource endpoint without a path or query")
	}
	if key == "" && credential == nil {
		return nil, errors.New("document analysis requires an API key or an explicit Azure identity")
	}
	u.Path = ""
	return &Client{
		endpoint: u, key: key, credential: credential,
		gate: make(chan struct{}, 1), pollDelay: 2 * time.Second,
		http: &http.Client{
			Timeout:       45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 2,
			},
		},
	}, nil
}

// Analyze processes at most 100 pages. The extra sentinel page detects longer
// documents instead of silently ingesting only their first 100 pages.
func (c *Client) Analyze(ctx context.Context, data []byte) (pages []Page, err error) {
	if len(data) == 0 || len(data) > MaxInput {
		return nil, errors.New("document analysis input must be between 1 byte and 25 MiB")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	target := *c.endpoint
	target.Path = modelPath + ":analyze"
	target.RawQuery = url.Values{
		"api-version": {apiVersion}, "outputContentFormat": {"markdown"},
		"stringIndexType": {"unicodeCodePoint"}, "pages": {"1-101"},
	}.Encode()
	// Never replay a billable POST automatically after an ambiguous failure.
	resp, err := c.request(ctx, http.MethodPost, target.String(), data)
	if err != nil {
		return nil, err
	}
	location, delay := resp.Header.Get("Operation-Location"), c.retryDelay(resp)
	statusErr := consumeStatus(resp, http.StatusAccepted)
	_ = resp.Body.Close() // Submission response is fully handled before polling.
	if statusErr != nil {
		return nil, statusErr
	}
	operation, err := c.operationURL(location)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer stop()
		resp, deleteErr := c.request(cleanup, http.MethodDelete, operation, nil)
		if deleteErr == nil {
			deleteErr = consumeStatus(resp, http.StatusNoContent)
			_ = resp.Body.Close() // Cleanup response has no content.
		}
		if deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("document analysis result cleanup failed (Azure retention may apply): %w", deleteErr))
			pages = nil
		}
	}()
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		resp, err := c.request(ctx, http.MethodGet, operation, nil)
		if err != nil {
			return nil, err
		}
		delay = c.retryDelay(resp)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			_ = resp.Body.Close() // Transient poll response is not reused.
			continue
		}
		if resp.StatusCode != http.StatusOK {
			statusErr := consumeStatus(resp, http.StatusOK)
			_ = resp.Body.Close() // Error details have been consumed within a size limit.
			return nil, statusErr
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
		_ = resp.Body.Close() // Read-only HTTP response.
		if readErr != nil || len(raw) > 32<<20 {
			return nil, errors.New("document analysis response is unreadable or exceeds 32 MiB")
		}
		var result analysisResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, errors.New("document analysis returned invalid JSON")
		}
		switch result.Status {
		case "notStarted", "running":
			continue
		case "succeeded":
			return result.pages()
		case "failed", "canceled":
			return nil, errors.New("azure document analysis failed; check document validity and resource diagnostics")
		default:
			return nil, errors.New("document analysis returned an unknown operation status")
		}
	}
}

func (c *Client) request(ctx context.Context, method, target string, data []byte) (*http.Response, error) {
	// #nosec G704 -- target is the configured Azure origin or a same-origin validated operation URL; no user-supplied URLs.
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("cannot create document analysis request")
	}
	if c.key != "" {
		req.Header.Set("Ocp-Apim-Subscription-Key", c.key)
	} else {
		token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://cognitiveservices.azure.com/.default"}})
		if err != nil {
			return nil, errors.New("document analysis identity authentication failed")
		}
		req.Header.Set("Authorization", "Bearer "+token.Token)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	// #nosec G704 -- New restricts the endpoint, operationURL checks origin/path, and redirects are disabled.
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("document analysis HTTP request failed; check connectivity (submission is not automatically retried)")
	}
	return resp, nil
}

func consumeStatus(resp *http.Response, expected int) error {
	if resp.StatusCode != expected {
		var failure struct {
			Error struct {
				Code  string `json:"code"`
				Inner struct {
					Code string `json:"code"`
				} `json:"innererror"`
			} `json:"error"`
		}
		// Expose only bounded error codes, never document text or provider messages.
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&failure); err == nil {
			return fmt.Errorf("document analysis HTTP %d (%s/%s); check permissions, quota and document validity",
				resp.StatusCode, safeCode(failure.Error.Code), safeCode(failure.Error.Inner.Code))
		}
		return fmt.Errorf("document analysis HTTP %d (expected %d); check permissions, quota and document validity", resp.StatusCode, expected)
	}

	return nil
}

func safeCode(code string) string {
	if code == "" || len(code) > 64 {
		return "unknown"
	}
	for _, char := range code {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		return "unknown"
	}
	return code
}

func (c *Client) retryDelay(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err == nil && seconds > 0 {
		return min(time.Duration(min(seconds, 10))*time.Second, 10*time.Second)
	}
	return c.pollDelay
}

func (c *Client) operationURL(location string) (string, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != c.endpoint.Scheme || u.Host != c.endpoint.Host ||
		u.User != nil || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("document analysis returned an untrusted operation URL")
	}
	id, ok := strings.CutPrefix(u.Path, modelPath+"/analyzeResults/")
	if !ok || id == "" || strings.ContainsAny(id, "/\\.%") {
		return "", errors.New("document analysis returned an invalid operation path")
	}
	for _, char := range id {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' {
			continue
		}
		return "", errors.New("document analysis returned an invalid operation identifier")
	}
	u.RawQuery = url.Values{"api-version": {apiVersion}}.Encode()
	return u.String(), nil
}

type analysisResult struct {
	Status string `json:"status"`
	Result struct {
		Content string `json:"content"`
		Pages   []struct {
			Number int `json:"pageNumber"`
			Spans  []struct {
				Offset int `json:"offset"`
				Length int `json:"length"`
			} `json:"spans"`
		} `json:"pages"`
	} `json:"analyzeResult"`
}

func (r analysisResult) pages() ([]Page, error) {
	if len(r.Result.Pages) == 0 || len(r.Result.Pages) > MaxPages {
		return nil, fmt.Errorf("document analysis requires 1-%d pages; no partial document was saved", MaxPages)
	}
	if len(r.Result.Content) > maxOutput || !utf8.ValidString(r.Result.Content) || strings.TrimSpace(r.Result.Content) == "" {
		return nil, errors.New("document analysis returned empty, invalid or oversized text")
	}
	content := []rune(r.Result.Content)
	pages := make([]Page, 0, len(r.Result.Pages))
	lastEnd, total := 0, 0
	for i, page := range r.Result.Pages {
		if page.Number != i+1 {
			return nil, errors.New("document analysis returned incomplete or unordered pages")
		}
		var text strings.Builder
		for _, span := range page.Spans {
			if span.Offset < lastEnd || span.Offset > len(content) || span.Length < 0 || span.Length > len(content)-span.Offset {
				return nil, errors.New("document analysis returned invalid page spans")
			}
			if strings.TrimSpace(string(content[lastEnd:span.Offset])) != "" {
				return nil, errors.New("document analysis page spans omit content")
			}
			part := string(content[span.Offset : span.Offset+span.Length])
			total += len(part)
			if total > maxOutput {
				return nil, errors.New("document analysis extracted text exceeds 8 MiB")
			}
			text.WriteString(part)
			lastEnd = span.Offset + span.Length
		}
		pages = append(pages, Page{Number: page.Number, Text: strings.TrimSpace(text.String())})
	}
	if total == 0 || strings.TrimSpace(string(content[lastEnd:])) != "" {
		return nil, errors.New("document analysis returned no page text")
	}
	return pages, nil
}
