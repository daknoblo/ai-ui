package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func TestChatAutomaticallyDelegatesImageRequests(t *testing.T) {
	for _, test := range []struct {
		name, chatModel, action string
		sourceImage             bool
		wantImages              int64
		wantError               bool
	}{
		{"responses image generation", "gpt-6-astra", "generate", false, 1, false},
		{"chat completion image generation", "gpt-4o", "generate", false, 1, false},
		{"responses image edit", "gpt-6-astra", "edit", true, 1, false},
		{"ordinary text stays text", "gpt-6-astra", "chat", false, 0, false},
		{"missing source does not generate a replacement", "gpt-6-astra", "edit", false, 0, true},
		{"malformed tool arguments", "gpt-6-astra", "invalid", false, 0, true},
		{"multiple image calls are not executed", "gpt-6-astra", "multiple", false, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var imageCalls, chatCalls atomic.Int64
			azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" || r.Header.Get("api-key") != "" {
					t.Error("request lost the configured identity")
				}
				if strings.Contains(r.URL.Path, "/images/") {
					imageCalls.Add(1)
					if r.URL.Query().Get("api-version") != "preview" {
						t.Error("image request lost its preview API version")
					}
					var model, prompt string
					if strings.HasSuffix(r.URL.Path, "/edits") {
						if err := r.ParseMultipartForm(1 << 20); err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = r.MultipartForm.RemoveAll() }() // Remove local test upload files.
						model, prompt = r.FormValue("model"), r.FormValue("prompt")
					} else {
						var body struct {
							Model  string `json:"model"`
							Prompt string `json:"prompt"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						model, prompt = body.Model, body.Prompt
					}
					if model != "image-backend" || prompt != "A blue park" {
						t.Errorf("wrong image delegation: model=%s prompt=%s", model, prompt)
					}
					if (test.action == "edit") != strings.HasSuffix(r.URL.Path, "/edits") {
						t.Error("image edit/generation routing is incorrect")
					}
					if err := json.NewEncoder(w).Encode(map[string]any{
						"data":  []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString([]byte("local-image"))}},
						"usage": map[string]int{"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
					}); err != nil {
						t.Error(err)
					}
					return
				}
				chatCalls.Add(1)
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if !strings.Contains(string(request["tools"]), "generate_image") ||
					string(request["model"]) != `"smart-chat"` {
					t.Error("chat was not told about the configured image tool")
				}
				responses := test.chatModel == "gpt-6-astra"
				if responses != strings.HasSuffix(r.URL.Path, "/responses") {
					t.Errorf("wrong tool-calling API: %s", r.URL.Path)
				}
				arguments := `{"prompt":"A blue park","edit":false}`
				switch test.action {
				case "edit":
					arguments = `{"prompt":"A blue park","edit":true}`
				case "invalid":
					arguments = `{"prompt":"","edit":false}`
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if responses {
					var output []map[string]any
					if test.action == "chat" {
						_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"A normal answer.\"}\n\n")
					} else {
						output = append(output, map[string]any{"type": "function_call", "call_id": "call-1", "name": "generate_image", "arguments": arguments})
						if test.action == "multiple" {
							output = append(output, map[string]any{"type": "function_call", "call_id": "call-2", "name": "generate_image", "arguments": arguments})
						}
					}
					event := map[string]any{"type": "response.completed", "response": map[string]any{
						"status": "completed", "model": test.chatModel, "output": output,
						"usage": map[string]int{"input_tokens": 5, "output_tokens": 5, "total_tokens": 10},
					}}
					data, err := json.Marshal(event)
					if err != nil {
						t.Error(err)
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				} else {
					chunk := map[string]any{"choices": []map[string]any{{
						"delta": map[string]any{"tool_calls": []map[string]any{{
							"index": 0, "id": "call-1", "type": "function",
							"function": map[string]string{"name": "generate_image", "arguments": arguments},
						}}},
						"finish_reason": "tool_calls",
					}}}
					data, err := json.Marshal(chunk)
					if err != nil {
						t.Error(err)
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
				}
			}))
			t.Cleanup(azure.Close)
			server, handler := newConfiguredServer(t, "en", config.Keys{}, config.Overrides{}, nil)
			snapshot := foundry.Snapshot{
				ResourceID: serverResourceID, Endpoint: azure.URL + "/openai/v1",
				Deployments: []foundry.Deployment{
					{Name: "smart-chat", ModelName: test.chatModel, ModelFormat: "OpenAI", ProvisioningState: "Succeeded"},
					{Name: "image-backend", ModelName: "gpt-image-2", ModelFormat: "OpenAI", ProvisioningState: "Succeeded"},
				},
			}
			server.cfg.ConfigureFoundry(serverResourceID, &serverFoundrySource{snapshot: snapshot}, nil)
			if err := server.cfg.SetCatalog(snapshot); err != nil {
				t.Fatal(err)
			}
			cfg := server.cfg.Get()
			cfg.ChatDeployment, cfg.ImageDeployment = "smart-chat", "image-backend"
			if err := server.cfg.Save(cfg); err != nil {
				t.Fatal(err)
			}
			id, err := server.store.CreateChat(t.Context(), "Image test", "smart-chat", "auto")
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range []struct{ role, text string }{
				{"user", "Earlier"}, {"assistant", "Earlier answer"}, {"user", "Create or change an image"},
			} {
				if _, err := server.store.AddMessage(t.Context(), id, message.role, message.text); err != nil {
					t.Fatal(err)
				}
			}
			if test.sourceImage {
				if _, err := server.store.AddImage(t.Context(), id, storage.ImageGenerated, "source.png", "", "image/png", []byte("source")); err != nil {
					t.Fatal(err)
				}
			}
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d/generate", id), nil))
			if imageCalls.Load() != test.wantImages || chatCalls.Load() != 1 {
				t.Fatalf("calls: chat=%d image=%d; %s", chatCalls.Load(), imageCalls.Load(), result.Body.String())
			}
			chat, err := server.store.GetChat(t.Context(), id)
			if err != nil || chat.Mode != storage.ChatModeChat || chat.Model != "smart-chat" {
				t.Fatal("automatic delegation changed the conversation's mode or text model")
			}
			if test.wantImages > 0 && !strings.Contains(result.Body.String(), "/images/") {
				t.Fatal("generated image was not returned inline")
			}
			if test.action == "chat" && !strings.Contains(result.Body.String(), "A normal answer.") {
				t.Fatal("ordinary chat was not preserved")
			}
			if test.wantError && !strings.Contains(result.Body.String(), "⚠") {
				t.Fatal("image routing failure was hidden")
			}
		})
	}
}
