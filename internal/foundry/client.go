package foundry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	apiVersion       = "2024-10-01"
	armScope         = "https://management.azure.com/.default"
	inferenceScope   = "https://cognitiveservices.azure.com/.default"
	discoveryTimeout = 45 * time.Second
	maxResponseBytes = 4 << 20
	maxCatalogBytes  = 16 << 20
	maxPages         = 100
	maxDeployments   = 10000
	maxAttempts      = 3
	maxRetryDelay    = 10 * time.Second
)

type Client struct {
	resourceID  string
	credential  azcore.TokenCredential
	httpClient  *http.Client
	armEndpoint string
	timeout     time.Duration
}

func New(identity Identity) (*Client, error) {
	resourceID := strings.TrimSuffix(strings.TrimSpace(identity.ResourceID), "/")
	if err := ValidateResourceID(resourceID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(identity.TenantID) == "" || strings.TrimSpace(identity.ClientID) == "" ||
		strings.TrimSpace(identity.ClientSecret) == "" {
		return nil, errors.New("azure resource identity requires an explicit tenant ID, client ID, and client secret")
	}
	credential, err := azidentity.NewClientSecretCredential(
		strings.TrimSpace(identity.TenantID), strings.TrimSpace(identity.ClientID), identity.ClientSecret,
		&azidentity.ClientSecretCredentialOptions{
			ClientOptions: azcore.ClientOptions{
				Cloud: cloud.AzurePublic,
				Retry: policy.RetryOptions{
					MaxRetries: 2, TryTimeout: 10 * time.Second,
					RetryDelay: time.Second, MaxRetryDelay: 4 * time.Second,
				},
			},
		},
	)
	if err != nil {
		// SDK identity errors can contain credential or response details.
		return nil, errors.New("azure client secret identity configuration is invalid; check tenant and client identifiers")
	}
	return newClient(resourceID, credential), nil
}

func newClient(resourceID string, credential azcore.TokenCredential) *Client {
	return &Client{
		resourceID: resourceID, credential: credential,
		armEndpoint: "https://management.azure.com", timeout: discoveryTimeout,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
	}
}

func (c *Client) Authorize(req *http.Request) error {
	if req == nil {
		return errors.New("azure inference authorization requires an HTTP request")
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for name := range req.Header {
		if strings.EqualFold(name, "api-key") || strings.EqualFold(name, "Authorization") {
			delete(req.Header, name)
		}
	}
	token, err := c.token(req.Context(), inferenceScope, "inference authentication")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (c *Client) Refresh(ctx context.Context, endpointOverride string) (Snapshot, error) {
	if c == nil || c.credential == nil || ctx == nil {
		return Snapshot{}, errors.New("azure deployment discovery requires an initialized identity and context")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	accountURL, err := c.resourceURL(c.resourceID)
	if err != nil {
		return Snapshot{}, err
	}
	budget := int64(maxCatalogBytes)
	var account struct {
		ID         string            `json:"id"`
		Properties accountProperties `json:"properties"`
	}
	if err := c.getJSON(ctx, accountURL, "account discovery", &account, &budget); err != nil {
		return Snapshot{}, err
	}
	if !strings.EqualFold(strings.TrimSuffix(account.ID, "/"), c.resourceID) {
		return Snapshot{}, errors.New("azure ARM account response does not match the configured resource ID")
	}
	endpoint, err := selectEndpoint(account.Properties, endpointOverride)
	if err != nil {
		return Snapshot{}, err
	}
	next, err := c.resourceURL(c.resourceID + "/deployments")
	if err != nil {
		return Snapshot{}, err
	}
	deployments := make([]Deployment, 0)
	seenPages, seenNames := make(map[string]bool), make(map[string]bool)
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return Snapshot{}, errors.New("azure deployment discovery exceeds the page limit")
		}
		if seenPages[next] {
			return Snapshot{}, errors.New("azure deployment discovery returned a repeated pagination link")
		}
		seenPages[next] = true
		var response struct {
			Value    *[]armDeployment `json:"value"`
			NextLink string           `json:"nextLink"`
		}
		if err := c.getJSON(ctx, next, "deployment discovery", &response, &budget); err != nil {
			return Snapshot{}, err
		}
		if response.Value == nil {
			return Snapshot{}, errors.New("azure ARM deployment response is missing its deployment array")
		}
		if len(deployments)+len(*response.Value) > maxDeployments {
			return Snapshot{}, errors.New("azure deployment discovery exceeds the deployment limit")
		}
		for _, raw := range *response.Value {
			if !validSegment(raw.Name) || seenNames[strings.ToLower(raw.Name)] {
				return Snapshot{}, errors.New("azure deployment response contains an invalid or duplicate deployment name")
			}
			if raw.ID != "" && !strings.EqualFold(strings.TrimSuffix(raw.ID, "/"), c.resourceID+"/deployments/"+raw.Name) {
				return Snapshot{}, errors.New("azure deployment response contains a deployment outside the configured account")
			}
			seenNames[strings.ToLower(raw.Name)] = true
			deployments = append(deployments, Deployment{
				ID: raw.ID, Name: raw.Name, ModelName: raw.Properties.Model.Name,
				ModelVersion: raw.Properties.Model.Version, ModelFormat: raw.Properties.Model.Format,
				ProvisioningState: raw.Properties.ProvisioningState, SKU: raw.SKU.Name,
				Capabilities: raw.Properties.Capabilities,
			})
		}
		if response.NextLink == "" {
			next = ""
		} else {
			next, err = c.paginationURL(next, response.NextLink)
			if err != nil {
				return Snapshot{}, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("azure deployment discovery interrupted: %w", err)
	}
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].Name < deployments[j].Name })
	return Snapshot{
		ResourceID: c.resourceID, Endpoint: endpoint, Deployments: deployments,
		RefreshedAt: time.Now().UTC(),
	}, nil
}

type armDeployment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	SKU  struct {
		Name string `json:"name"`
	} `json:"sku"`
	Properties struct {
		Model struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Format  string `json:"format"`
		} `json:"model"`
		ProvisioningState string            `json:"provisioningState"`
		Capabilities      map[string]string `json:"capabilities"`
	} `json:"properties"`
}

func (c *Client) resourceURL(path string) (string, error) {
	u, err := url.Parse(c.armEndpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("azure ARM client endpoint configuration is invalid")
	}
	u.Path, u.RawPath = path, ""
	u.RawQuery = url.Values{"api-version": {apiVersion}}.Encode()
	return u.String(), nil
}

func (c *Client) paginationURL(current, raw string) (string, error) {
	invalid := errors.New("azure ARM pagination link must use HTTPS and remain on the same ARM host and account deployment path")
	if len(raw) > 16384 {
		return "", invalid
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", invalid
	}
	next, err := url.Parse(raw)
	if err != nil || next.User != nil || next.Fragment != "" || next.Opaque != "" {
		return "", invalid
	}
	next = base.ResolveReference(next)
	expectedPath := (&url.URL{Path: c.resourceID + "/deployments"}).EscapedPath()
	if next.Scheme != "https" || !strings.EqualFold(next.Host, base.Host) ||
		!strings.EqualFold(next.Path, c.resourceID+"/deployments") || !strings.EqualFold(next.EscapedPath(), expectedPath) {
		return "", invalid
	}
	query, err := url.ParseQuery(next.RawQuery)
	if err != nil {
		return "", invalid
	}
	if versions, present := query["api-version"]; present && (len(versions) != 1 || versions[0] != apiVersion) {
		return "", invalid
	}
	query.Set("api-version", apiVersion)
	next.RawQuery = query.Encode()
	return next.String(), nil
}

func (c *Client) getJSON(ctx context.Context, endpoint, action string, target any, budget *int64) error {
	return c.getJSONWithScope(ctx, endpoint, "ARM "+action, armScope, target, budget)
}

func (c *Client) getJSONWithScope(ctx context.Context, endpoint, action, scope string, target any, budget *int64) error {
	authentication := "management authentication"
	if scope == inferenceScope {
		authentication = "inference authentication"
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		token, err := c.token(ctx, scope, authentication)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return errors.New("azure " + action + " request construction failed")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		client := *c.httpClient
		// Never forward a bearer token through a redirect, even with an injected client.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		response, err := client.Do(req)
		if err != nil {
			return safeError(ctx, err, "azure "+action+" request")
		}
		if response.StatusCode != http.StatusOK {
			err := responseError("azure "+action, response.StatusCode, response.Header)
			// Error bodies may contain sensitive details; do not read or propagate them.
			_ = response.Body.Close()
			if !retryable(response.StatusCode) || attempt+1 >= maxAttempts {
				return err
			}
			delay, allowed := retryDelay(response.Header, attempt, time.Now())
			if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
				allowed = false
			}
			if !allowed {
				return err
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("azure %s retry interrupted: %w", action, ctx.Err())
			case <-timer.C:
			}
			continue
		}
		body, err := readResponse(ctx, response, *budget)
		if err != nil {
			return err
		}
		*budget -= int64(len(body))
		if err := json.Unmarshal(body, target); err != nil {
			return errors.New("azure " + action + " response is not valid JSON with the expected field types")
		}
		return nil
	}
	return errors.New("azure " + action + " retry limit exceeded")
}

func readResponse(ctx context.Context, response *http.Response, budget int64) ([]byte, error) {
	defer func() {
		// Read errors are handled below; a read-only response has no close result to recover.
		_ = response.Body.Close()
	}()
	limit := min(int64(maxResponseBytes), budget)
	if response.ContentLength > limit {
		return nil, errors.New("azure response exceeds the catalog size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, safeError(ctx, err, "azure response read")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("azure response exceeds the catalog size limit")
	}
	return body, nil
}

func (c *Client) token(ctx context.Context, scope, action string) (string, error) {
	if c == nil || c.credential == nil {
		return "", errors.New("azure authorization requires an initialized client secret identity")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("azure %s interrupted: %w", action, err)
	}
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", safeError(ctx, err, "azure "+action)
		}
		var authenticationError *azidentity.AuthenticationFailedError
		if errors.As(err, &authenticationError) && authenticationError.RawResponse != nil {
			response := authenticationError.RawResponse
			return "", responseError("azure "+action, response.StatusCode, response.Header)
		}
		var sdkError *azcore.ResponseError
		if errors.As(err, &sdkError) {
			var headers http.Header
			if sdkError.RawResponse != nil {
				headers = sdkError.RawResponse.Header
			}
			return "", responseError("azure "+action, sdkError.StatusCode, headers)
		}
		return "", errors.New("azure " + action + " failed; verify the explicit client secret identity configuration")
	}
	if token.Token == "" {
		return "", errors.New("azure " + action + " returned an empty access token")
	}
	return token.Token, nil
}

func safeError(ctx context.Context, err error, action string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s interrupted: %w", action, ctx.Err())
	}
	for _, interruption := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, interruption) {
			return fmt.Errorf("%s interrupted: %w", action, interruption)
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return errors.New(action + " timed out")
	}
	return errors.New(action + " failed")
}

func responseError(action string, status int, headers http.Header) error {
	detail := fmt.Sprintf("%s failed: HTTP %d", action, status)
	for _, key := range []string{"x-ms-request-id", "request-id", "x-ms-correlation-request-id"} {
		if requestID := headers.Get(key); safeRequestID(requestID) {
			detail += " (request ID " + requestID + ")"
			break
		}
	}
	switch status {
	case http.StatusUnauthorized:
		detail += "; the service rejected the identity token"
	case http.StatusForbidden:
		detail += "; verify identity permissions at the configured account resource scope"
	case http.StatusTooManyRequests:
		detail += "; the service rate limit was reached"
	}
	return errors.New(detail)
}

func safeRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !asciiAlphanumeric(r) && !strings.ContainsRune("-_.:", r) {
			return false
		}
	}
	return true
}

func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(headers http.Header, attempt int, now time.Time) (time.Duration, bool) {
	var delay time.Duration
	raw := headers.Get("Retry-After")
	if seconds, err := strconv.ParseUint(raw, 10, 32); err == nil {
		delay = time.Duration(seconds) * time.Second
	} else if errors.Is(err, strconv.ErrRange) {
		return 0, false
	} else if when, err := http.ParseTime(raw); err == nil {
		delay = max(0, when.Sub(now))
	} else {
		delay = 200 * time.Millisecond * time.Duration(1<<attempt)
	}
	return delay, delay <= maxRetryDelay
}
