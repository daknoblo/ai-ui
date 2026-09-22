package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/storage"
)

// TestImageMarkdown makes sure the prompt cannot break out of the Markdown
// image syntax.
func TestImageMarkdown(t *testing.T) {
	got := imageMarkdown(7, "a [cat](evil) on\na roof")
	want := "![a catevil on a roof](/images/7)"
	if got != want {
		t.Errorf("imageMarkdown = %q, want %q", got, want)
	}
}

// TestImageParam falls back to the first allowed value for unknown input.
func TestImageParam(t *testing.T) {
	if got := imageParam("LOW", imageQualities); got != "low" {
		t.Errorf("imageParam = %q, want low", got)
	}
	if got := imageParam("gigantic", imageSizes); got != imageSizes[0] {
		t.Errorf("imageParam = %q, want the fallback %q", got, imageSizes[0])
	}
}

func TestImageRefinementsUseLastSuccessfulResult(t *testing.T) {
	for _, mode := range []string{"manual", "chat-completions", "responses"} {
		t.Run(mode, func(t *testing.T) {
			type imageRequest struct {
				edit         bool
				source       []byte
				name, mime   string
				outputFormat string
			}
			requests := make(chan imageRequest, 5)
			var imageCalls, chatCalls atomic.Int64
			outputs := make([][]byte, 5)
			formats := []string{"png", "jpeg", "png", "png", "jpeg"}
			for i, format := range formats {
				picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
				picture.Set(0, 0, color.RGBA{R: []uint8{0, 40, 80, 120, 160}[i], G: 90, B: 180, A: 255})
				var buf bytes.Buffer
				var err error
				if format == "jpeg" {
					err = jpeg.Encode(&buf, picture, nil)
				} else {
					err = png.Encode(&buf, picture)
				}
				if err != nil {
					t.Fatal(err)
				}
				outputs[i] = buf.Bytes()
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/images/") {
					edit := chatCalls.Add(1) > 1
					args := fmt.Sprintf(`{"prompt":"Refine the picture","edit":%t}`, edit)
					var event any
					if mode == "responses" {
						if !strings.HasSuffix(r.URL.Path, "/responses") {
							t.Errorf("expected Responses API, got %s", r.URL.Path)
						}
						event = map[string]any{"type": "response.completed", "response": map[string]any{
							"status": "completed", "model": "gpt-6-astra",
							"output": []map[string]string{{"type": "function_call", "call_id": "image-call",
								"name": "generate_image", "arguments": args}},
						}}
					} else {
						if mode != "chat-completions" || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
							t.Errorf("unexpected chat request: mode=%s path=%s", mode, r.URL.Path)
						}
						event = map[string]any{"choices": []map[string]any{{
							"delta": map[string]any{"tool_calls": []map[string]any{{
								"index": 0, "id": "image-call", "type": "function",
								"function": map[string]string{"name": "generate_image", "arguments": args},
							}}}, "finish_reason": "tool_calls",
						}}}
					}
					payload, err := json.Marshal(event)
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload) // Local test stream.
					return
				}
				n := imageCalls.Add(1)
				observed := imageRequest{edit: strings.HasSuffix(r.URL.Path, "/edits")}
				if observed.edit {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = r.MultipartForm.RemoveAll() }() // Remove local test upload files.
					file, header, err := r.FormFile("image")
					if err != nil {
						t.Error(err)
						return
					}
					observed.source, err = io.ReadAll(file)
					_ = file.Close() // In-memory test upload.
					if err != nil {
						t.Error(err)
						return
					}
					observed.name, observed.mime = header.Filename, header.Header.Get("Content-Type")
					observed.outputFormat = r.FormValue("output_format")
				} else {
					var body struct {
						OutputFormat string `json:"output_format"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					observed.outputFormat = body.OutputFormat
				}
				requests <- observed
				if n == 3 {
					http.Error(w, `{"error":{"message":"Simulated image rejection"}}`, http.StatusBadRequest)
					return
				}
				if n > int64(len(outputs)) {
					t.Error("unexpected duplicate image request")
					http.Error(w, "unexpected request", http.StatusInternalServerError)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(outputs[n-1])}},
				}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(backend.Close)
			s, handler, chatID := generationTestServer(t, backend.URL+"/openai/v1")
			cfg := s.cfg.Get()
			cfg.ImageDeployment = "image-model"
			if mode == "responses" {
				cfg.ChatDeployment, cfg.ChatModels = "gpt-6-astra", []string{"gpt-6-astra"}
				if err := s.store.UpdateChatModel(t.Context(), chatID, cfg.ChatDeployment); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			otherChat, err := s.store.CreateChat(t.Context(), "Other chat", "", "auto")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.AddMessage(t.Context(), otherChat, "user", "Keep this separate conversation"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.AddImage(t.Context(), chatID, storage.ImageUpload, "original.png", "", "image/png", outputs[3]); err != nil {
				t.Fatal(err)
			}
			latest, err := s.store.LatestImage(t.Context(), chatID)
			if err != nil {
				t.Fatal(err)
			}
			successes := 0
			for i, format := range formats {
				// Newer image IDs in a different chat must never become the source.
				if _, err := s.store.AddImage(t.Context(), otherChat, storage.ImageGenerated, "", "", "image/png", outputs[0]); err != nil {
					t.Fatal(err)
				}
				cfg.ImageFormat = format
				if err := s.cfg.Save(cfg); err != nil {
					t.Fatal(err)
				}
				form := url.Values{"message": {fmt.Sprintf("Image step %d", i+1)}}
				if mode == "manual" {
					form.Set("mode", "image")
					if i > 0 {
						form.Set("edit", "1")
					}
				}
				var submitted *httptest.ResponseRecorder
				if i == 3 {
					turns, err := s.store.ListGenerations(t.Context(), chatID)
					if err != nil || len(turns) != 3 {
						t.Fatalf("failed turn missing: %+v, %v", turns, err)
					}
					submitted = retryRequest(t, handler, chatID, turns[2].ResponseID, form.Encode())
				} else {
					submitted = sendGeneration(t, handler, chatID, form.Encode())
				}
				streamURL := submittedGenerationURL(t, submitted)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, streamURL, nil))
				s.jobs.Wait()
				var observed imageRequest
				select {
				case observed = <-requests:
				case <-time.After(3 * time.Second):
					t.Fatalf("step %d did not reach the image endpoint: %s", i+1, response.Body.String())
				}
				if observed.edit != (i > 0) || observed.outputFormat != format {
					t.Fatalf("step %d: edit=%t format=%s", i+1, observed.edit, observed.outputFormat)
				}
				if i > 0 {
					name := "source.png"
					if latest.MIME == "image/jpeg" {
						name = "source.jpg"
					}
					if !bytes.Equal(observed.source, latest.Data) || observed.mime != latest.MIME || observed.name != name {
						t.Fatalf("step %d did not send the exact latest image with matching MIME and filename", i+1)
					}
				}
				stored, err := s.store.LatestImage(t.Context(), chatID)
				if err != nil {
					t.Fatal(err)
				}
				if i == 2 {
					if stored.ID != latest.ID || !bytes.Equal(stored.Data, latest.Data) ||
						!strings.Contains(response.Body.String(), "Simulated image rejection") {
						t.Fatal("failed generation changed the edit source or hid the error")
					}
				} else {
					successes++
					if stored.ID <= latest.ID || stored.Kind != storage.ImageGenerated || !bytes.Equal(stored.Data, outputs[i]) ||
						!strings.Contains(response.Body.String(), fmt.Sprintf("/images/%d", stored.ID)) {
						t.Fatalf("step %d did not store and display its new image", i+1)
					}
					latest = stored
				}
				imageResponse := httptest.NewRecorder()
				handler.ServeHTTP(imageResponse, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/images/%d", latest.ID), nil))
				if imageResponse.Code != http.StatusOK || !bytes.Equal(imageResponse.Body.Bytes(), latest.Data) ||
					imageResponse.Header().Get("Content-Type") != latest.MIME {
					t.Fatalf("step %d: displayed image differs from stored edit source", i+1)
				}
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, streamURL, nil))
				count, err := s.store.CountImages(t.Context(), chatID)
				if err != nil || count != successes+1 || imageCalls.Load() != int64(i+1) {
					t.Fatalf("replay duplicated images or requests: count=%d calls=%d err=%v", count, imageCalls.Load(), err)
				}
				page := httptest.NewRecorder()
				handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", chatID), nil))
				if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), fmt.Sprintf("/images/%d", latest.ID)) {
					t.Fatal("reopening the conversation lost its latest image")
				}
			}
		})
	}
}
