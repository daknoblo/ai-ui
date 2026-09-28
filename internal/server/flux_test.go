package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func fluxServer(t *testing.T, endpoint, language string) (*Server, http.Handler, int64) {
	t.Helper()
	s, handler := newConfiguredServer(t, language, config.Keys{}, config.Overrides{}, nil)
	catalog := foundry.Snapshot{ResourceID: serverResourceID, Endpoint: endpoint + "/openai/v1"}
	for _, entry := range []struct{ name, model, format string }{
		{"text", "gpt-4o", "OpenAI"}, {"gpt", "gpt-image-2", "OpenAI"},
		{"pro", "FLUX.2-pro", "BlackForestLabs"}, {"flex", "FLUX.2-flex", "BlackForestLabs"},
	} {
		catalog.Deployments = append(catalog.Deployments, foundry.Deployment{Name: entry.name, ModelName: entry.model,
			ModelFormat: entry.format, ModelVersion: "1", ProvisioningState: "Succeeded"})
	}
	s.cfg.ConfigureFoundry(serverResourceID, &serverFoundrySource{snapshot: catalog}, nil)
	if err := s.cfg.SetCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{foundry.Chat: {catalog.Key(catalog.Deployments[0])}}
	for _, op := range []foundry.Operation{foundry.Images, foundry.ImageEdits} {
		for _, d := range catalog.Deployments[1:] {
			cfg.EnabledDeployments[op] = append(cfg.EnabledDeployments[op], catalog.Key(d))
		}
	}
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	id, err := s.store.CreateChat(t.Context(), "FLUX test", "text", "auto")
	if err != nil {
		t.Fatal(err)
	}
	return s, handler, id
}

func TestFluxComposerSelectionValidationAndLocalization(t *testing.T) {
	for _, language := range []string{"en", "de"} {
		t.Run(language, func(t *testing.T) {
			s, handler, id := fluxServer(t, "https://local.services.ai.azure.com", language)
			selectModel := func(model string) *httptest.ResponseRecorder {
				return postFoundryForm(handler, fmt.Sprintf("/chat/%d/image-model", id), url.Values{"image_model": {model}})
			}
			if response := selectModel("flux.2-flex"); response.Code != http.StatusNoContent {
				t.Fatal(response.Body.String())
			}
			chat, err := s.store.GetChat(t.Context(), id)
			if err != nil || chat.Mode != storage.ChatModeChat || chat.ImageModel != "flux.2-flex" || chat.Model != "text" {
				t.Fatalf("image picker changed text selection or mode: %+v %v", chat, err)
			}
			page := httptest.NewRecorder()
			handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
			body := page.Body.String()
			picker, input := strings.Index(body, `id="image-model-select"`), strings.Index(body, `id="chat-form"`)
			if picker < 0 || input < picker || !strings.Contains(body, s.t("chat.image_automatic")) ||
				!strings.Contains(body, `<option value="flux.2-flex" selected>`) || !strings.Contains(body, s.t("chat.image_model_help")) {
				t.Fatal("localized saved image selector is not above the composer")
			}
			before := s.cfg.Get()
			for field, value := range map[string]string{"flux_steps": "51", "flux_guidance": "NaN", "flux_size": "9999x9999", "image_size": "bad", "image_quality": "unsafe", "image_format": "svg"} {
				response := postFoundryForm(handler, "/image/params", url.Values{field: {value}})
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), s.t("error.image_parameters")) {
					t.Fatalf("%s validation = %d %s", field, response.Code, response.Body.String())
				}
			}
			if after := s.cfg.Get(); after.FluxSteps != before.FluxSteps || after.ImageFormat != before.ImageFormat {
				t.Fatal("invalid parameters changed saved options")
			}
			cfg := s.cfg.Get()
			cfg.EnabledDeployments[foundry.Images] = cfg.EnabledDeployments[foundry.Images][:2]
			if err := s.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			if response := selectModel("flux.2-flex"); response.Code != http.StatusBadRequest {
				t.Fatal("unavailable model was accepted")
			}
			if response := sendGeneration(t, handler, id, "message=test&mode=image"); response.Code != http.StatusBadRequest {
				t.Fatal("stale saved model silently changed")
			}
			if response := selectModel(""); response.Code != http.StatusNoContent {
				t.Fatal("automatic selection failed")
			}
		})
	}
}

func TestFluxDurableBytesFailureRetryAndLatestSource(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aVr0AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}

	type request struct {
		Model, Prompt, InputImage string
		Steps                     int
	}
	requests := make(chan request, 4)
	var calls atomic.Int64
	results := map[int64][]byte{1: append(bytes.Clone(png), '1'), 2: append(bytes.Clone(png), '2'), 4: append(bytes.Clone(png), '4')}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model      string `json:"model"`
			Prompt     string `json:"prompt"`
			InputImage string `json:"input_image"`
			Steps      int    `json:"steps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- request{body.Model, body.Prompt, body.InputImage, body.Steps}
		n := calls.Add(1)
		if n == 3 {
			http.Error(w, "provider rejected image", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(results[n])) // Local response fixture.
	}))
	t.Cleanup(backend.Close)
	s, handler, id := fluxServer(t, backend.URL, "en")
	if err := s.store.UpdateChatImageModel(t.Context(), id, "flux.2-pro"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddImage(t.Context(), id, storage.ImageUpload, "source.png", "", "image/png", png); err != nil {
		t.Fatal(err)
	}
	complete := func(response *httptest.ResponseRecorder) {
		t.Helper()
		stream := submittedGenerationURL(t, response)
		for range 2 {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, stream, nil))
		}
		s.jobs.Wait()
	}
	var previous int64
	for i := int64(1); i <= 3; i++ {
		complete(sendGeneration(t, handler, id, fmt.Sprintf("message=Edit+%d&mode=image&edit=1", i)))
		last, err := s.store.LatestImage(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if !bytes.Equal(last.Data, results[i]) {
				t.Fatal("durable provider image bytes changed")
			}
			previous = last.ID
		} else if last.ID != previous || !bytes.Equal(last.Data, results[2]) {
			t.Fatal("failed edit replaced the latest successful source")
		}
	}
	turns, err := s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 3 {
		t.Fatal(err)
	}
	var failed storage.Generation
	for _, turn := range turns {
		if turn.State == storage.GenerationFailed {
			failed = turn
		}
	}
	if failed.ID == 0 {
		t.Fatal("provider failure was not durable")
	}
	complete(retryRequest(t, handler, id, failed.ID,
		"mode=image&edit=1&image_model=flux.2-flex&flux_steps=21&flux_guidance=4.5&flux_size=2048x2048"))
	for i := 1; i <= 4; i++ {
		req := <-requests
		data, err := base64.StdEncoding.DecodeString(req.InputImage)
		expected := png
		if i == 2 {
			expected = results[1]
		} else if i >= 3 {
			expected = results[2]
		}
		if err != nil || !bytes.Equal(data, expected) {
			t.Fatalf("request %d did not use exact latest successful bytes", i)
		}
		if i == 4 && (req.Model != "flex" || req.Steps != 21 || req.Prompt != "Edit 3") {
			t.Fatalf("retry lost current model/settings or original prompt: %+v", req)
		}
	}
	turns, err = s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 4 || calls.Load() != 4 {
		t.Fatal("replays caused extra image generation")
	}
	for _, turn := range turns {
		var opts generationOptions
		if err := json.Unmarshal([]byte(turn.Request), &opts); err != nil {
			t.Fatal(err)
		}
		if opts.ImageOptions.Model == "flux.2-flex" && (opts.ImageOptions.Steps != 21 ||
			opts.ImageOptions.Size != "2048x2048" || opts.SourceImageID != previous) {
			t.Fatal("durable generation lost frozen image settings/source")
		}
	}
}

func TestFluxImageToolUsesImageSelectionWithoutChangingChatModel(t *testing.T) {
	const image = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aVr0AAAAASUVORK5CYII="
	var images, chats atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.HasPrefix(r.URL.Path, "/providers/blackforestlabs/") {
			images.Add(1)
			if string(body["model"]) != `"flex"` || !strings.HasSuffix(r.URL.Path, "/flux-2-flex") {
				t.Error("image tool did not use the selected image model")
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, image) // Local inline-image fixture.
			return
		}
		chats.Add(1)
		if string(body["model"]) != `"text"` || !strings.Contains(string(body["tools"]), "generate_image") {
			t.Error("image selection changed the chat model or removed its image tool")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"image\",\"type\":\"function\",\"function\":{\"name\":\"generate_image\",\"arguments\":\"{\\\"prompt\\\":\\\"A tree\\\",\\\"edit\\\":false}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n") // Local tool-call fixture.
	}))
	t.Cleanup(backend.Close)
	s, handler, id := fluxServer(t, backend.URL, "en")
	response := postFoundryForm(handler, fmt.Sprintf("/chat/%d/image-model", id), url.Values{"image_model": {"flux.2-flex"}})
	if response.Code != http.StatusNoContent {
		t.Fatal(response.Body.String())
	}
	stream := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Draw+a+tree"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, stream, nil))
	s.jobs.Wait()
	img, err := s.store.LatestImage(t.Context(), id)
	expected, decodeErr := base64.StdEncoding.DecodeString(image)
	if err != nil || decodeErr != nil || !bytes.Equal(img.Data, expected) || images.Load() != 1 || chats.Load() != 1 {
		t.Fatalf("tool did not persist exact image bytes: %v %v calls=%d/%d", err, decodeErr, images.Load(), chats.Load())
	}
	chat, err := s.store.GetChat(t.Context(), id)
	if err != nil || chat.Mode != storage.ChatModeChat || chat.Model != "text" || chat.ImageModel != "flux.2-flex" {
		t.Fatal("image tool changed saved chat selection")
	}
}
