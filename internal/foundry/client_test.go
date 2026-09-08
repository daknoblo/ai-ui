package foundry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

type fakeCredential struct {
	mu     sync.Mutex
	scopes []string
	err    error
}

func (f *fakeCredential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, options.Scopes...)
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return azcore.AccessToken{}, err
	}
	if len(options.Scopes) != 1 {
		return azcore.AccessToken{}, errors.New("expected exactly one token audience")
	}
	token := "offline-arm-token"
	if options.Scopes[0] == inferenceScope {
		token = "offline-inference-token"
	} else if options.Scopes[0] != armScope {
		return azcore.AccessToken{}, errors.New("unexpected token audience")
	}
	return azcore.AccessToken{Token: token, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func (f *fakeCredential) requestedScopes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.scopes...)
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *fakeCredential) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	credential := &fakeCredential{}
	client := newClient(testResourceID, credential)
	client.httpClient, client.armEndpoint, client.timeout = server.Client(), server.URL, 5*time.Second
	return client, credential
}

func accountJSON() string {
	return fmt.Sprintf(`{"id":%q,"properties":{"endpoint":"https://canonical.cognitiveservices.azure.com/","endpoints":{"Azure OpenAI Legacy API - Latest moniker":"https://canonical.openai.azure.com/"}}}`, testResourceID)
}

func deploymentJSON(name, model string) map[string]any {
	return map[string]any{
		"id": testResourceID + "/deployments/" + name, "name": name,
		"sku": map[string]any{"name": "GlobalStandard"},
		"properties": map[string]any{
			"model":             map[string]any{"name": model, "version": "2024-08-06", "format": "OpenAI"},
			"provisioningState": "Succeeded", "capabilities": map[string]string{"chatCompletion": "true"},
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write fixture: %v", err)
	}
}

func writeAccount(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, accountJSON()); err != nil {
		t.Errorf("write account fixture: %v", err)
	}
}

func TestRefreshPaginationAndTokenAudiences(t *testing.T) {
	var paths []string
	client, credential := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer offline-arm-token" || r.Header.Get("api-key") != "" {
			t.Error("ARM discovery used the wrong authentication")
		}
		if r.URL.Query().Get("api-version") != apiVersion {
			t.Error("unexpected ARM API version")
		}
		switch r.URL.Path {
		case testResourceID:
			writeAccount(t, w)
		case testResourceID + "/deployments":
			if r.URL.Query().Get("$skiptoken") == "" {
				writeJSON(t, w, map[string]any{
					"value":    []any{deploymentJSON("vectors", "text-embedding-3-small"), deploymentJSON("future", "unknown-model")},
					"nextLink": testResourceID + "/deployments?api-version=" + apiVersion + "&%24skiptoken=opaque%2Bcursor",
				})
			} else {
				if r.URL.Query().Get("$skiptoken") != "opaque+cursor" {
					t.Error("pagination cursor changed")
				}
				writeJSON(t, w, map[string]any{"value": []any{deploymentJSON("customer-alias", "gpt-4o")}})
			}
		default:
			t.Errorf("discovery escaped the account scope: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	start := time.Now()
	snapshot, err := client.Refresh(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || len(snapshot.Deployments) != 3 || snapshot.ResourceID != testResourceID ||
		snapshot.Endpoint != "https://canonical.openai.azure.com/openai/v1" ||
		snapshot.RefreshedAt.Before(start) || snapshot.RefreshedAt.After(time.Now()) {
		t.Fatalf("incomplete discovery snapshot: %+v", snapshot)
	}
	if got := snapshot.Names(Chat); !reflect.DeepEqual(got, []string{"customer-alias", "future"}) {
		t.Fatalf("chat names = %v", got)
	}
	deployment, found := snapshot.Find("customer-alias")
	if !found || deployment.ModelVersion != "2024-08-06" || deployment.SKU != "GlobalStandard" ||
		deployment.ModelFormat != "OpenAI" || deployment.ID != testResourceID+"/deployments/customer-alias" {
		t.Fatal("ARM model metadata was not preserved")
	}
	request := httptest.NewRequest(http.MethodPost, snapshot.Endpoint+"/chat/completions", nil)
	request.Header["api-key"] = []string{"old-key"}
	request.Header.Set("Authorization", "old-token")
	if err := client.Authorize(request); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") != "Bearer offline-inference-token" {
		t.Fatal("inference used the management token")
	}
	for name := range request.Header {
		if strings.EqualFold(name, "api-key") {
			t.Fatal("API key was not cleared")
		}
	}
	wantScopes := []string{armScope, armScope, armScope, inferenceScope}
	if got := credential.requestedScopes(); !reflect.DeepEqual(got, wantScopes) {
		t.Fatalf("requested audiences = %v", got)
	}
}

func TestRefreshHTTPStatusRetriesAndSanitization(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var attempts atomic.Int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Retry-After", "0")
				w.Header().Set("x-ms-request-id", "request-123")
				http.Error(w, "credential-sentinel-do-not-propagate", status)
			})
			snapshot, err := client.Refresh(context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(status)) ||
				!strings.Contains(err.Error(), "request-123") || strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("status was not preserved safely: %v", err)
			}
			wantAttempts := int32(1)
			if retryable(status) {
				wantAttempts = maxAttempts
			}
			if attempts.Load() != wantAttempts || snapshot.ResourceID != "" {
				t.Fatalf("attempts = %d; partial snapshot = %+v", attempts.Load(), snapshot)
			}
		})
	}
}

func TestRefreshTransientStatusThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testResourceID {
			if attempts.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
			} else {
				writeAccount(t, w)
			}
		} else {
			writeJSON(t, w, map[string]any{"value": []any{}})
		}
	})
	snapshot, err := client.Refresh(context.Background(), "https://canonical.services.ai.azure.com/")
	if err != nil || attempts.Load() != 2 || snapshot.Endpoint != "https://canonical.services.ai.azure.com/openai/v1" {
		t.Fatalf("transient recovery or override failed: %v", err)
	}
}

func TestRefreshRejectsMalformedOrIncompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name, account, deployments string
	}{
		{name: "invalid-account-json", account: `{"credential-sentinel`},
		{name: "wrong-account-type", account: `[]`},
		{name: "account-outside-scope", account: strings.Replace(accountJSON(), "test-account", "other-account", 1)},
		{name: "missing-account-id", account: `{"properties":{"endpoint":"https://canonical.openai.azure.com/"}}`},
		{name: "missing-endpoint", account: fmt.Sprintf(`{"id":%q,"properties":{}}`, testResourceID)},
		{name: "missing-deployments", deployments: `{}`},
		{name: "null-deployments", deployments: `{"value":null}`},
		{name: "wrong-deployment-type", deployments: `{"value":{}}`},
		{name: "invalid-deployment-json", deployments: `{"value":[`},
		{name: "multiple-json-values", deployments: `{"value":[]}{"value":[]}`},
		{name: "invalid-capability-value", deployments: `{"value":[{"name":"model","properties":{"capabilities":{"chatCompletion":true}}}]}`},
		{name: "missing-deployment-name", deployments: `{"value":[{"properties":{}}]}`},
		{name: "invalid-deployment-name", deployments: `{"value":[{"name":"../other"}]}`},
		{name: "duplicate-names", deployments: `{"value":[{"name":"model"},{"name":"MODEL"}]}`},
		{name: "deployment-outside-scope", deployments: `{"value":[{"name":"model","id":"/other/account/deployments/model"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				body := test.deployments
				if r.URL.Path == testResourceID {
					body = test.account
					if body == "" {
						body = accountJSON()
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, body); err != nil {
					t.Errorf("write invalid fixture: %v", err)
				}
			})
			snapshot, err := client.Refresh(context.Background(), "")
			if err == nil || snapshot.ResourceID != "" || strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("malformed data was not rejected safely: %v", err)
			}
		})
	}
}

func TestRefreshRejectsOversizedResponses(t *testing.T) {
	for _, declaredLength := range []bool{false, true} {
		t.Run(strconv.FormatBool(declaredLength), func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				body := strings.Repeat(" ", maxResponseBytes+1)
				if declaredLength {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				} else {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				// The client deliberately closes oversized response bodies early.
				_, _ = io.WriteString(w, body)
			})
			if _, err := client.Refresh(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("oversized response accepted: %v", err)
			}
		})
	}
}

func TestRefreshRejectsHostilePagination(t *testing.T) {
	for _, link := range []string{
		"https://attacker.invalid/steal?credential-sentinel",
		"http://HOST" + testResourceID + "/deployments",
		"https://user:credential-sentinel@HOST" + testResourceID + "/deployments",
		"https://HOST" + strings.Replace(testResourceID, "test-account", "other-account", 1) + "/deployments",
		"https://HOST/subscriptions/11111111-2222-3333-4444-555555555555/resources",
		"https://HOST" + testResourceID + "/deployments#credential-sentinel",
		"https://HOST" + testResourceID + "/%64eployments",
		"?api-version=wrong",
		"?api-version=" + apiVersion + "&api-version=wrong",
		"?%xx=credential-sentinel",
	} {
		t.Run(link, func(t *testing.T) {
			var requests atomic.Int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == testResourceID {
					writeAccount(t, w)
				} else {
					next := strings.ReplaceAll(link, "HOST", r.Host)
					writeJSON(t, w, map[string]any{"value": []any{deploymentJSON("partial", "gpt-4o")}, "nextLink": next})
				}
			})
			snapshot, err := client.Refresh(context.Background(), "")
			if err == nil || snapshot.ResourceID != "" || requests.Load() != 2 || strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("unsafe pagination was not stopped: requests=%d, err=%v", requests.Load(), err)
			}
		})
	}
}

func TestRefreshDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var redirected atomic.Int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == testResourceID {
					http.Redirect(w, r, "https://"+r.Host+"/redirect-target?credential-sentinel", status)
				} else {
					redirected.Add(1)
					w.WriteHeader(http.StatusOK)
				}
			})
			if _, err := client.Refresh(context.Background(), ""); err == nil || redirected.Load() != 0 ||
				strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("bearer redirect was not blocked safely: %v", err)
			}
		})
	}
}

func TestRefreshPaginationAndCatalogLimits(t *testing.T) {
	for _, mode := range []string{"loop", "pages", "deployments", "total-bytes", "late-failure"} {
		t.Run(mode, func(t *testing.T) {
			var pages atomic.Int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == testResourceID {
					writeAccount(t, w)
					return
				}
				page := pages.Add(1)
				value := []any{}
				next := "?page=" + strconv.Itoa(int(page))
				var padding string
				switch mode {
				case "loop":
					next = "?api-version=" + apiVersion
				case "deployments":
					for i := 0; i <= maxDeployments; i++ {
						value = append(value, map[string]string{"name": "model-" + strconv.Itoa(i)})
					}
				case "total-bytes":
					padding = strings.Repeat("x", 3<<20)
				case "late-failure":
					if page == 2 {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					value = []any{deploymentJSON("partial", "gpt-4o")}
				}
				// Size-limit tests intentionally close the response before it is fully written.
				_ = json.NewEncoder(w).Encode(map[string]any{"value": value, "nextLink": next, "padding": padding})
			})
			snapshot, err := client.Refresh(context.Background(), "")
			if err == nil || snapshot.ResourceID != "" || pages.Load() > maxPages {
				t.Fatalf("unbounded or partial catalog: pages=%d, err=%v", pages.Load(), err)
			}
			expected := map[string]string{
				"loop": "repeated", "pages": "page limit", "deployments": "deployment limit",
				"total-bytes": "size limit", "late-failure": "HTTP 403",
			}
			if !strings.Contains(err.Error(), expected[mode]) {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}

func TestRefreshCancellationAndRetryDeadline(t *testing.T) {
	t.Run("request-timeout", func(t *testing.T) {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		client.timeout = 30 * time.Millisecond
		if _, err := client.Refresh(context.Background(), ""); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("discovery timeout was not honored: %v", err)
		}
	})
	t.Run("retry-cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			cancel()
		})
		if _, err := client.Refresh(ctx, ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("retry cancellation was not honored: %v", err)
		}
	})
	t.Run("retry-after-beyond-deadline", func(t *testing.T) {
		var attempts atomic.Int32
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		if _, err := client.Refresh(context.Background(), ""); err == nil || attempts.Load() != 1 {
			t.Fatal("Retry-After beyond the deadline must not be retried early")
		}
	})
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if delay, allowed := retryDelay(http.Header{"Retry-After": {now.Add(3 * time.Second).Format(http.TimeFormat)}}, 0, now); !allowed || delay != 3*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v, %v", delay, allowed)
	}
	if _, allowed := retryDelay(http.Header{"Retry-After": {"999999999999999999999999"}}, 0, now); allowed {
		t.Fatal("overflowing Retry-After must not trigger an early retry")
	}
}

func TestCredentialFailuresAreSanitized(t *testing.T) {
	const marker = "credential-sentinel-do-not-propagate"
	response := &http.Response{
		StatusCode: http.StatusUnauthorized, Header: http.Header{"X-Ms-Request-Id": {"auth-request-id"}},
		Body: io.NopCloser(strings.NewReader(marker)),
	}
	for _, failure := range []error{
		errors.New(marker),
		&azidentity.AuthenticationFailedError{RawResponse: response},
		&azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: marker, RawResponse: response},
		fmt.Errorf("%s: %w", marker, context.Canceled),
	} {
		credential := &fakeCredential{err: failure}
		client := newClient(testResourceID, credential)
		request := httptest.NewRequest(http.MethodPost, "https://canonical.openai.azure.com/openai/v1/chat/completions", nil)
		request.Header.Set("api-key", marker)
		request.Header.Set("Authorization", marker)
		err := client.Authorize(request)
		if err == nil || strings.Contains(err.Error(), marker) || request.Header.Get("api-key") != "" || request.Header.Get("Authorization") != "" {
			t.Fatalf("identity error or stale credentials escaped sanitization: %v", err)
		}
		if errors.Is(failure, context.Canceled) && !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation identity lost")
		}
	}
	client := newClient(testResourceID, &fakeCredential{})
	if err := client.Authorize(nil); err == nil {
		t.Fatal("nil request accepted")
	}
	request := &http.Request{}
	if err := client.Authorize(request); err != nil || request.Header.Get("Authorization") == "" {
		t.Fatalf("nil request header not initialized: %v", err)
	}
}

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (f fakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRefreshTransportFailureIsSanitizedAndNotRetried(t *testing.T) {
	var calls int
	client := newClient(testResourceID, &fakeCredential{})
	client.httpClient = &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("transport failure with credential-sentinel in URL query")
	})}
	_, err := client.Refresh(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), "credential-sentinel") || calls != 1 {
		t.Fatalf("transport failure leaked details or retried without a transient status: %v", err)
	}
}

type credentialFunc func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (f credentialFunc) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return f(ctx, options)
}

func TestAuthorizeHonorsCredentialDeadlineAndCancellation(t *testing.T) {
	client := newClient(testResourceID, credentialFunc(func(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
		<-ctx.Done()
		return azcore.AccessToken{}, fmt.Errorf("credential-sentinel: %w", ctx.Err())
	}))
	client.timeout = 10 * time.Millisecond
	req := httptest.NewRequest(http.MethodPost, "https://canonical.openai.azure.com/openai/v1/chat/completions", nil)
	if err := client.Authorize(req); !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "credential-sentinel") {
		t.Fatalf("credential deadline was not preserved safely: %v", err)
	}
	credential := &fakeCredential{}
	client.credential = credential
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Authorize(req.WithContext(ctx)); !errors.Is(err, context.Canceled) || len(credential.requestedScopes()) != 0 {
		t.Fatal("an already canceled request acquired a token")
	}
}

func TestSDKCredentialCachesSeparateAudiences(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	const authority = "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token"
	requests := make(map[string]int)
	transport := fakeTransport(func(request *http.Request) (*http.Response, error) {
		var value any
		if strings.Contains(request.URL.Path, "/.well-known/openid-configuration") {
			value = map[string]any{
				"authorization_endpoint": "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/authorize",
				"token_endpoint":         authority, "issuer": "https://login.microsoftonline.com/" + tenant + "/v2.0",
			}
		} else if request.URL.String() == authority {
			if err := request.ParseForm(); err != nil {
				return nil, errors.New("invalid offline token request")
			}
			scope := request.Form.Get("scope")
			requests[scope]++
			value = map[string]any{
				"access_token": "offline-cached-token", "token_type": "Bearer", "expires_in": 3600,
				"ext_expires_in": 3600,
			}
		} else {
			return nil, errors.New("unexpected identity endpoint; network access is disabled")
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK, Request: request,
			Header: http.Header{"Content-Type": {"application/json"}},
			Body:   io.NopCloser(strings.NewReader(string(body))),
		}, nil
	})
	credential, err := azidentity.NewClientSecretCredential(
		tenant, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "offline-only-secret",
		&azidentity.ClientSecretCredentialOptions{
			DisableInstanceDiscovery: true,
			ClientOptions: azcore.ClientOptions{
				Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(testResourceID, credential)
	for _, scope := range []string{armScope, inferenceScope, armScope, inferenceScope} {
		if _, err := client.token(context.Background(), scope, "offline authentication"); err != nil {
			t.Fatal(err)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("SDK did not separate token audiences: %v", requests)
	}
	for scope, count := range requests {
		if count != 1 || (!strings.Contains(scope, armScope) && !strings.Contains(scope, inferenceScope)) {
			t.Fatalf("unexpected token requests: %v", requests)
		}
	}
}
