package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
)

const fluxPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aVr0AAAAASUVORK5CYII="

func TestFluxManualEndpointAndProbe(t *testing.T) {
	var calls atomic.Int64
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/providers/blackforestlabs/v1/flux-2-pro" ||
			r.Header.Get("Authorization") != "Bearer image-test-key" || r.Header.Get("api-key") != "" ||
			body["model"] != "FLUX.2-pro" {
			t.Error("manual FLUX request lost its configured endpoint, model or dedicated credentials")
		}
		if body["prompt"] == nil {
			if len(body) != 1 {
				t.Error("probe submitted generation parameters")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"prompt is required"}}`) // Local validation fixture.
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, fluxPNG) // Local image fixture.
	}))
	t.Cleanup(azure.Close)
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"), config.Keys{API: "chat-test-key", Image: "image-test-key"}, config.Overrides{})
	cfg := config.Defaults()
	cfg.Endpoint, cfg.ImageEndpoint, cfg.ImageDeployment = "https://chat.example/openai/v1", azure.URL, "FLUX.2-pro"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	client := New(store)
	if err := client.VerifyImage(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GenerateImage(t.Context(), "A tree", ImageOptions{}); err != nil || calls.Load() != 2 {
		t.Fatalf("manual generation failed: %v calls=%d", err, calls.Load())
	}
}

func TestFluxAzureInlineGenerationEditAndFrozenRoute(t *testing.T) {
	for _, model := range []string{"flux.2-pro", "flux.2-flex"} {
		for _, edit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/edit=%t", model, edit), func(t *testing.T) {
				image, err := base64.StdEncoding.DecodeString(fluxPNG)
				if err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int64
				azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/providers/blackforestlabs/v1/"+strings.Replace(model, "flux.2-", "flux-2-", 1) ||
						r.URL.RawQuery != "api-version=preview" || r.Header.Get("Authorization") == "" || r.Header.Get("api-key") != "" {
						t.Errorf("wrong Azure provider request: %s %v", r.URL, r.Header)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["model"] != "image-alias" || body["prompt"] != "Draw a tree" ||
						body["width"] != float64(2048) || body["height"] != float64(2048) ||
						body["num_images"] != float64(1) || body["output_format"] != "png" {
						t.Errorf("wrong provider payload: %v", body)
					}
					for key := range body {
						if !slices.Contains([]string{"model", "prompt", "width", "height", "num_images", "output_format", "input_image", "steps", "guidance"}, key) {
							t.Errorf("unsupported or safety parameter: %s", key)
						}
					}
					if edit {
						data, err := base64.StdEncoding.DecodeString(fmt.Sprint(body["input_image"]))
						if err != nil || !bytes.Equal(data, image) {
							t.Error("source image bytes changed")
						}
					} else if _, exists := body["input_image"]; exists {
						t.Error("generation unexpectedly included an edit source")
					}
					if model == "flux.2-flex" {
						if body["steps"] != float64(35) || body["guidance"] != 4.5 {
							t.Error("flex settings lost")
						}
					} else if body["steps"] != nil || body["guidance"] != nil {
						t.Error("Pro received Flex-only parameters")
					}
					if _, err := fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, fluxPNG); err != nil {
						t.Error(err)
					}
				}))
				t.Cleanup(azure.Close)
				client, store := identityClient(t, azure.URL)
				catalog := store.FoundryStatus().Catalog
				d := foundry.Deployment{Name: "image-alias", ModelName: model, ModelFormat: "Black Forest Labs", ModelVersion: "1", ProvisioningState: "Succeeded"}
				catalog.Deployments = append(catalog.Deployments, d)
				if err := store.SetCatalog(catalog); err != nil {
					t.Fatal(err)
				}
				cfg := store.Get()
				cfg.EnabledDeployments = map[foundry.Operation][]string{foundry.Images: {catalog.Key(d)}, foundry.ImageEdits: {catalog.Key(d)}}
				if err := store.Save(cfg); err != nil {
					t.Fatal(err)
				}
				opts, err := client.PrepareImage(ImageOptions{Deployment: model, FluxSize: "2048x2048", Quality: "high", Format: "png", Steps: 35, Guidance: 4.5}, edit)
				if err != nil {
					t.Fatal(err)
				}
				cfg.EnabledDeployments = map[foundry.Operation][]string{}
				if err := store.Save(cfg); err != nil {
					t.Fatal(err)
				}
				var result ImageResult
				if edit {
					result, err = client.EditImage(t.Context(), "Draw a tree", ImageSource{MIME: "image/png", Data: image}, opts)
				} else {
					result, err = client.GenerateImage(t.Context(), "Draw a tree", opts)
				}
				if err != nil || !bytes.Equal(result.Data, image) || result.MIME != "image/png" || calls.Load() != 1 {
					t.Fatalf("result changed or retried: %+v %v calls=%d", result, err, calls.Load())
				}
				if _, err := client.GenerateImage(t.Context(), "Draw a tree", ImageOptions{Deployment: model}); err == nil || calls.Load() != 1 {
					t.Fatal("stale model silently routed to a different deployment")
				}
			})
		}
	}
}

func TestFluxRejectsURLsAsyncRedirectsAndInvalidParameters(t *testing.T) {
	for _, body := range []string{
		"redirect",
		"failure",
		`{"data":[{"url":"http://127.0.0.1/private"}]}`,
		`{"id":"job","polling_url":"http://169.254.169.254/metadata"}`,
		`{"status":"Ready","result":{"sample":"https://attacker.example/image"}}`,
		`{"data":[{"b64_json":"aGVsbG8="}]}`,
		`{"data":[{"b64_json":"!"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int64
			azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if body == "redirect" {
					http.Redirect(w, r, "/private", http.StatusTemporaryRedirect)
					return
				}
				if body == "failure" {
					http.Error(w, "upstream failure", http.StatusServiceUnavailable)
					return
				}
				_, _ = fmt.Fprint(w, body) // Bounded local response fixture.
			}))
			t.Cleanup(azure.Close)
			client, store := identityClient(t, azure.URL)
			catalog := store.FoundryStatus().Catalog
			d := foundry.Deployment{Name: "draw", ModelName: "FLUX.2-pro", ModelFormat: "BlackForestLabs", ProvisioningState: "Succeeded"}
			catalog.Deployments[len(catalog.Deployments)-1] = d
			if err := store.SetCatalog(catalog); err != nil {
				t.Fatal(err)
			}
			if _, err := client.GenerateImage(t.Context(), "tree", ImageOptions{}); err == nil || calls.Load() != 1 {
				t.Fatal("untrusted response accepted or fetched")
			}
			for _, opts := range []ImageOptions{
				{FluxSize: "8192x8192"}, {Format: "svg"}, {Steps: 51}, {Guidance: 1}, {FluxSize: "1025x1024"},
			} {
				if _, err := client.GenerateImage(t.Context(), "tree", opts); err == nil || calls.Load() != 1 {
					t.Fatal("invalid parameters reached provider")
				}
			}
		})
	}
}
