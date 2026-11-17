package documentai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New("https://test.cognitiveservices.azure.com", "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.endpoint, err = url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.pollDelay = time.Millisecond
	return client
}

func TestAnalyzePagesAndCleanup(t *testing.T) {
	var posts, polls, deletes atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Ocp-Apim-Subscription-Key") != "test-key" {
			t.Error("missing resource-scoped key")
		}
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != "%PDF-test" || r.URL.Query().Get("pages") != "1-101" ||
				r.URL.Query().Get("outputContentFormat") != "markdown" || r.URL.Query().Get("stringIndexType") != "unicodeCodePoint" ||
				r.Header.Get("Content-Type") != "application/octet-stream" || r.URL.Query().Has("features") {
				t.Error("incorrect bounded Layout request")
			}
			w.Header().Set("Operation-Location", "http://"+r.Host+modelPath+"/analyzeResults/test-id?api-version="+apiVersion)
			w.WriteHeader(http.StatusAccepted)
		case http.MethodGet:
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{"content":"Ä €\nB","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":4}]},{"pageNumber":2,"spans":[{"offset":4,"length":1}]}]}}`)
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	pages, err := client.Analyze(t.Context(), []byte("%PDF-test"))
	if err != nil || len(pages) != 2 || pages[0].Text != "Ä €" || pages[1].Text != "B" {
		t.Fatalf("pages=%+v error=%v", pages, err)
	}
	if posts.Load() != 1 || polls.Load() != 2 || deletes.Load() != 1 {
		t.Fatal("submission, polling or cleanup count changed")
	}
}

func TestAnalyzeFailuresNeverReturnPartialSuccess(t *testing.T) {
	for _, mode := range []string{"post-failure", "foreign-operation", "redirect", "failed", "invalid-json", "missing-page", "too-many-pages", "bad-span", "cleanup-failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var posts, deletes atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					posts.Add(1)
					if mode == "post-failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					location := "http://" + r.Host + modelPath + "/analyzeResults/test"
					if mode == "foreign-operation" {
						location = "https://attacker.invalid/steal"
					}
					if mode == "redirect" {
						w.Header().Set("Location", "https://attacker.invalid/steal")
						w.WriteHeader(http.StatusTemporaryRedirect)
						return
					}
					w.Header().Set("Operation-Location", location)
					w.WriteHeader(http.StatusAccepted)
				case http.MethodDelete:
					deletes.Add(1)
					if mode == "cleanup-failure" {
						w.WriteHeader(http.StatusForbidden)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				case http.MethodGet:
					switch mode {
					case "cancel":
						cancel()
						_, _ = io.WriteString(w, `{"status":"running"}`)
					case "failed":
						_, _ = io.WriteString(w, `{"status":"failed","error":{"message":"sensitive provider detail"}}`)
					case "invalid-json":
						_, _ = io.WriteString(w, `{`)
					case "missing-page":
						_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{"content":"X","pages":[{"pageNumber":2,"spans":[{"offset":0,"length":1}]}]}}`)
					case "bad-span":
						_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{"content":"X","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":2}]}]}}`)
					case "too-many-pages":
						pages := make([]map[string]int, MaxPages+1)
						for i := range pages {
							pages[i] = map[string]int{"pageNumber": i + 1}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"status": "succeeded", "analyzeResult": map[string]any{"content": "X", "pages": pages}})
					default:
						_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{"content":"X","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":1}]}]}}`)
					}
				}
			})
			pages, err := client.Analyze(ctx, []byte("%PDF-test"))
			if err == nil || pages != nil || strings.Contains(err.Error(), "sensitive provider detail") {
				t.Fatalf("expected safe explicit failure, got %+v %v", pages, err)
			}
			wantDelete := int32(1)
			if mode == "post-failure" || mode == "foreign-operation" || mode == "redirect" {
				wantDelete = 0
			}
			if posts.Load() != 1 || deletes.Load() != wantDelete {
				t.Fatalf("posts=%d deletes=%d", posts.Load(), deletes.Load())
			}
		})
	}
}

func TestEndpointAndOperationValidation(t *testing.T) {
	for _, endpoint := range []string{"http://test.cognitiveservices.azure.com", "https://example.com", "https://test.cognitiveservices.azure.com.evil.test", "https://key@test.cognitiveservices.azure.com", "https://test.cognitiveservices.azure.com/path", "https://test.cognitiveservices.azure.com?key=secret"} {
		if _, err := New(endpoint, "secret", nil); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	client, err := New("https://test.cognitiveservices.azure.com/", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/other", modelPath + "/analyzeResults/../other", modelPath + "/analyzeResults/%2e%2e", modelPath + "/analyzeResults/abc/other"} {
		if _, err := client.operationURL("https://test.cognitiveservices.azure.com" + path); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if _, err := New("https://test.cognitiveservices.azure.com", "", nil); err == nil {
		t.Fatal("accepted missing credentials")
	}
}

func TestInputLimitsBeforeBilling(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid input reached Azure")
	})
	for _, input := range [][]byte{nil, make([]byte, MaxInput+1)} {
		if _, err := client.Analyze(t.Context(), input); err == nil {
			t.Fatal("accepted invalid upload size")
		}

	}
}

type testCredential struct{ t *testing.T }

func (c testCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if len(options.Scopes) != 1 || options.Scopes[0] != "https://cognitiveservices.azure.com/.default" {
		c.t.Error("incorrect Document Intelligence token audience")
	}
	return azcore.AccessToken{Token: "synthetic-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestBearerAuthentication(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Ocp-Apim-Subscription-Key") != "" {
			t.Error("bearer identity was not used exclusively")
		}
		http.Error(w, "synthetic rejection", http.StatusBadRequest)
	})
	client.key, client.credential = "", testCredential{t: t}
	if _, err := client.Analyze(t.Context(), []byte("%PDF-test")); err == nil {
		t.Fatal("expected synthetic rejection")
	}
}
