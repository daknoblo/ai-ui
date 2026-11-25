package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestSunburstGenerationEditAndOptionIsolation(t *testing.T) {
	source, err := base64.StdEncoding.DecodeString(fluxPNG)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []bool{false, true} {
		t.Run(fmt.Sprint(edit), func(t *testing.T) {
			calls := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") == "" {
					t.Error("identity missing")
				}
				if edit {
					if r.URL.Path != "/openai/v1/images/edits" {
						t.Error(r.URL.Path)
					}
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = r.MultipartForm.RemoveAll() }()
					if r.FormValue("model") != "image-alias" || r.FormValue("quality") != "max" || r.FormValue("size") != "1536x864" {
						t.Error("edit lost canonical options")
					}
					f, _, err := r.FormFile("image")
					if err != nil {
						t.Error(err)
						return
					}
					data, err := io.ReadAll(f)
					_ = f.Close() // Read-only local test upload.
					if err != nil || !bytes.Equal(data, source) {
						t.Error("edit source changed")
					}
				} else {
					var body imageRequest
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if r.URL.Path != "/openai/v1/images/generations" || body.Model != "image-alias" || body.Quality != "max" || body.Size != "1536x864" {
						t.Errorf("incorrect generation: %+v", body)
					}
				}
				_, _ = fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, fluxPNG)
			}))
			defer backend.Close()
			client, store := identityClient(t, backend.URL)
			catalog := store.FoundryStatus().Catalog
			sun := foundry.Deployment{Name: "image-alias", ModelName: SunburstModel, ModelVersion: "2026-09-08", ModelFormat: "OpenAI", ProvisioningState: "Succeeded"}
			pro := foundry.Deployment{Name: "pro", ModelName: "FLUX.2-pro", ModelVersion: "1", ModelFormat: "Black Forest Labs", ProvisioningState: "Succeeded"}
			catalog.Deployments = append(catalog.Deployments, sun, pro)
			if err := store.SetCatalog(catalog); err != nil {
				t.Fatal(err)
			}
			cfg := store.Get()
			cfg.EnabledDeployments = map[foundry.Operation][]string{foundry.Images: {catalog.Key(sun), catalog.Key(pro)}, foundry.ImageEdits: {catalog.Key(sun), catalog.Key(pro)}}
			if err := store.Save(cfg); err != nil {
				t.Fatal(err)
			}
			input := ImageOptions{Deployment: SunburstModel, Size: "1024x1024", Quality: "low", SunburstSize: "1536x864", SunburstQuality: "max", FluxSize: "2048x2048", Format: "png"}
			opts, err := client.PrepareImage(input, edit)
			if err != nil {
				t.Fatal(err)
			}
			input.Deployment = "flux.2-pro"
			flux, err := client.PrepareImage(input, edit)
			if err != nil || flux.Size != "2048x2048" || flux.Quality != "" {
				t.Fatalf("Sunburst leaked into FLUX: %+v %v", flux, err)
			}
			cfg.EnabledDeployments = map[foundry.Operation][]string{}
			if err := store.Save(cfg); err != nil {
				t.Fatal(err)
			}
			var result ImageResult
			if edit {
				result, err = client.EditImage(t.Context(), "Edit", ImageSource{Data: source, MIME: "image/png"}, opts)
			} else {
				result, err = client.GenerateImage(t.Context(), "Draw", opts)
			}
			if err != nil || !bytes.Equal(result.Data, source) || calls != 1 {
				t.Fatalf("frozen request failed: %v calls=%d", err, calls)
			}
		})
	}
}

func TestSunburstOptionsDoNotExpandLegacyGPT(t *testing.T) {
	for _, quality := range []string{"xhigh", "max"} {
		if err := ValidateImageOptions(ImageOptions{Model: SunburstModel, Size: "2560x1440", Quality: quality}); err != nil {
			t.Fatal(err)
		}
		if err := ValidateImageOptions(ImageOptions{Model: "gpt-image-2", Quality: quality}); err == nil {
			t.Fatal("Sunburst quality accepted by legacy GPT")
		}
	}
	for _, opts := range []ImageOptions{{Model: SunburstModel, Size: "16384x16384"}, {Model: SunburstModel, Quality: "unsafe"}} {
		if err := ValidateImageOptions(opts); err == nil {
			t.Fatal("invalid Sunburst options accepted")
		}
	}
}
