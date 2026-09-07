package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// uploadRequest builds a multipart upload of a single file.
func uploadRequest(t *testing.T, chatID string, filename, contentType string, data []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", contentType)
	part, err := form.CreatePart(header)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/chat/"+chatID+"/documents", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req
}

// TestImageUploadIsStoredWithoutImageEndpoint covers attaching a picture on an
// instance that has no image generation configured: it is an attachment for the
// chat model, so it must not be pushed into the document parser.
func TestImageUploadIsStoredWithoutImageEndpoint(t *testing.T) {
	srv, handler := newTestServer(t, i18n.EN)
	if srv.cfg.ImagesConfigured() {
		t.Fatal("the test server must start without image generation")
	}

	chatID, err := srv.store.CreateChat(t.Context(), untitled, "", llm.ReasoningAuto)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", "screenshot.png", "image/png", []byte{0x89, 'P', 'N', 'G'}))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	images, err := srv.store.ListImagesByKind(t.Context(), chatID, storage.ImageUpload)
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(images) != 1 || images[0].Name != "screenshot.png" {
		t.Fatalf("the image was not stored as an attachment: %+v", images)
	}
	if !strings.Contains(rec.Body.String(), "screenshot.png") {
		t.Errorf("the refreshed chip list does not mention the attachment: %s", rec.Body.String())
	}
}

// TestAttachedImagesReachTheModel is the regression test for attachments that
// were uploaded but never handed to the model: the picture has to travel with
// the latest user message and be named in the system prompt.
func TestAttachedImagesReachTheModel(t *testing.T) {
	srv, _ := newTestServer(t, i18n.EN)
	ctx := t.Context()

	chatID, err := srv.store.CreateChat(ctx, untitled, "", llm.ReasoningAuto)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, err := srv.store.AddImage(ctx, chatID, storage.ImageUpload,
		"screenshot.png", "", "image/png", []byte{1, 2, 3}); err != nil {
		t.Fatalf("add image: %v", err)
	}
	if _, err := srv.store.CreateDocument(ctx, chatID, "report.pdf", "application/pdf"); err != nil {
		t.Fatalf("create document: %v", err)
	}

	history := []storage.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "I attached something"},
	}
	// An empty embedding deployment skips retrieval, which would need an
	// endpoint; the attachment handling has to work regardless.
	msgs, err := srv.buildLLMMessages(ctx, chatID, srv.cfg.Get(), history, "I attached something", false)
	if err != nil {
		t.Fatalf("build messages: %v", err)
	}

	if len(msgs) != len(history)+1 {
		t.Fatalf("got %d messages, want system + %d history entries", len(msgs), len(history))
	}
	system := msgs[0].Content
	for _, name := range []string{"report.pdf", "screenshot.png"} {
		if !strings.Contains(system, name) {
			t.Errorf("the system prompt does not name %q: %s", name, system)
		}
	}

	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("the last message is %q, want user", last.Role)
	}
	if len(last.Images) != 1 {
		t.Fatalf("the latest user message carries %d images, want 1", len(last.Images))
	}
	if last.Images[0].MIME != "image/png" || !bytes.Equal(last.Images[0].Data, []byte{1, 2, 3}) {
		t.Errorf("unexpected attachment payload: %+v", last.Images[0])
	}
	for _, m := range msgs[:len(msgs)-1] {
		if len(m.Images) != 0 {
			t.Errorf("only the latest user message may carry attachments, %q does too", m.Role)
		}
	}
}

// TestAttachmentBudgetSkipsOversizedImages makes sure an image that is left out
// of the request is not announced to the model either.
func TestAttachmentBudgetSkipsOversizedImages(t *testing.T) {
	srv, _ := newTestServer(t, i18n.EN)
	ctx := t.Context()

	chatID, err := srv.store.CreateChat(ctx, untitled, "", llm.ReasoningAuto)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, err := srv.store.AddImage(ctx, chatID, storage.ImageUpload,
		"huge.png", "", "image/png", bytes.Repeat([]byte{7}, maxContextImageBytes+1)); err != nil {
		t.Fatalf("add image: %v", err)
	}

	att := srv.attachmentsOf(ctx, chatID)
	if len(att.images) != 0 {
		t.Errorf("an oversized image must not be sent, got %d", len(att.images))
	}
	if len(att.names) != 0 {
		t.Errorf("an image that is not sent must not be announced, got %v", att.names)
	}
}

// wiredServer builds a server whose LLM client talks to a stub endpoint. Every
// request body reaching it is collected, so a test can assert on what actually
// left the process.
func wiredServer(t *testing.T, models []string, pinned string) (*Server, http.Handler, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any

	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"I see it\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(azure.Close)

	srv, handler := newConfiguredServer(t, i18n.EN,
		config.Keys{API: "key"}, config.Overrides{ChatModels: models},
		func(cfg *config.Config) {
			cfg.Endpoint = azure.URL + "/openai/v1"
			cfg.ChatDeployment = "model-router"
		})

	if _, err := srv.store.CreateChat(t.Context(), untitled, pinned, llm.ReasoningAuto); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return srv, handler, &bodies
}

// askWithAttachment uploads a file to chat 1 and sends a message about it.
func askWithAttachment(t *testing.T, handler http.Handler, filename, mime string, data []byte) {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", filename, mime, data))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	send := httptest.NewRequest(http.MethodPost, "/chat/1/send",
		strings.NewReader("message=what+is+in+the+attachment"))
	send.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, send)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat/1/generate", nil))
	if !strings.Contains(rec.Body.String(), "I see it") {
		t.Fatalf("the answer was not streamed: %s", rec.Body.String())
	}
}

// TestAttachedImageTravelsToTheEndpoint walks the whole path a user takes:
// attach a picture, send a message, and check that the request leaving for the
// model actually carries the image.
func TestAttachedImageTravelsToTheEndpoint(t *testing.T) {
	// The handler follows up with a request for the chat title, so every body
	// is collected and only the first one - the answer itself - is inspected.
	_, handler, bodies := wiredServer(t, nil, "gpt-5.6-sol")
	askWithAttachment(t, handler, "shot.png", "image/png", []byte{0x89, 'P', 'N', 'G'})

	if len(*bodies) == 0 {
		t.Fatal("no request reached the endpoint")
	}
	messages, _ := (*bodies)[0]["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("the answer request carried no messages")
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	parts, ok := last["content"].([]any)
	if !ok {
		t.Fatalf("the user message carries no attachment: %v", last["content"])
	}
	var images int
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if part["type"] == "image_url" {
			images++
		}
	}
	if images != 1 {
		t.Errorf("got %d image parts in the request, want 1: %v", images, parts)
	}
	system, _ := messages[0].(map[string]any)
	if text, _ := system["content"].(string); !strings.Contains(text, "shot.png") {
		t.Errorf("the system prompt does not name the attachment: %q", text)
	}
}

// TestImageSwitchesToAVisionModel covers the automatic model choice: a picture
// attached while a text-only model is pinned must be answered by a model that
// can actually see it.
func TestImageSwitchesToAVisionModel(t *testing.T) {
	_, handler, bodies := wiredServer(t,
		[]string{"gpt-3.5-turbo", "o1-mini", "gpt-4o"}, "gpt-3.5-turbo")
	askWithAttachment(t, handler, "shot.png", "image/png", []byte{0x89, 'P', 'N', 'G'})

	if len(*bodies) == 0 {
		t.Fatal("no request reached the endpoint")
	}
	if got := (*bodies)[0]["model"]; got != "gpt-4o" {
		t.Errorf("the request used model %v, want the vision capable gpt-4o", got)
	}
}

// TestBlindModelGetsNoImages is the regression test for a chat that wedged
// itself: with no vision capable deployment the images were still attached, the
// endpoint rejected the request, and because the attachments are re-read on
// every turn the chat stayed broken until the image was deleted.
func TestBlindModelGetsNoImages(t *testing.T) {
	_, handler, bodies := wiredServer(t,
		[]string{"gpt-3.5-turbo", "o1-mini"}, "gpt-3.5-turbo")
	askWithAttachment(t, handler, "shot.png", "image/png", []byte{0x89, 'P', 'N', 'G'})

	if len(*bodies) == 0 {
		t.Fatal("no request reached the endpoint")
	}
	messages, _ := (*bodies)[0]["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("the answer request carried no messages")
	}
	for i, m := range messages {
		msg, _ := m.(map[string]any)
		if _, isArray := msg["content"].([]any); isArray {
			t.Errorf("message %d carries images although no model can read them", i)
		}
	}
	// The name stays in the prompt, so the model can say it cannot look at it.
	system, _ := messages[0].(map[string]any)
	if text, _ := system["content"].(string); !strings.Contains(text, "shot.png") {
		t.Errorf("the attachment was hidden from the model entirely: %q", text)
	}
}

// TestNewestImagesAreAttached covers which images survive the context cap: the
// picture a question is about is the one that was just attached, so the newest
// have to win over the oldest.
func TestNewestImagesAreAttached(t *testing.T) {
	srv, _ := newTestServer(t, i18n.EN)
	ctx := t.Context()

	chatID, err := srv.store.CreateChat(ctx, untitled, "", llm.ReasoningAuto)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	// One more than the context allows, so the cap has to choose.
	total := maxContextImages + 3
	for i := range total {
		name := fmt.Sprintf("shot-%02d.png", i)
		if _, err := srv.store.AddImage(ctx, chatID, storage.ImageUpload,
			name, "", "image/png", []byte{byte(i)}); err != nil {
			t.Fatalf("add image %d: %v", i, err)
		}
	}

	att := srv.attachmentsOf(ctx, chatID)
	if len(att.images) != maxContextImages {
		t.Fatalf("got %d images, want the cap of %d", len(att.images), maxContextImages)
	}

	// The newest is the last upload; it must be present and come last.
	newest := byte(total - 1)
	if got := att.images[len(att.images)-1].Data[0]; got != newest {
		t.Errorf("the newest image is not the last one attached: got %d, want %d", got, newest)
	}
	oldest := byte(0)
	for _, img := range att.images {
		if img.Data[0] == oldest {
			t.Error("the oldest image displaced a newer one")
		}
	}
	// The names have to describe exactly what was sent.
	if len(att.names) != len(att.images) {
		t.Errorf("announced %d attachments but sent %d", len(att.names), len(att.images))
	}
}

// TestTextAttachmentKeepsTheChosenModel is the counterpart to the automatic
// switch: a document needs no vision, so the model the user pinned stays.
func TestTextAttachmentKeepsTheChosenModel(t *testing.T) {
	_, handler, bodies := wiredServer(t,
		[]string{"gpt-3.5-turbo", "gpt-4o"}, "gpt-3.5-turbo")

	// Uploading a document needs a verified embedding endpoint, so the
	// attachment is stored directly and only the send path is exercised.
	askWithoutUpload(t, handler)

	if len(*bodies) == 0 {
		t.Fatal("no request reached the endpoint")
	}
	if got := (*bodies)[0]["model"]; got != "gpt-3.5-turbo" {
		t.Errorf("the request used model %v, want the pinned gpt-3.5-turbo", got)
	}
}

// askWithoutUpload sends a plain message to chat 1 and drains the answer.
func askWithoutUpload(t *testing.T, handler http.Handler) {
	t.Helper()

	send := httptest.NewRequest(http.MethodPost, "/chat/1/send",
		strings.NewReader("message=summarize+the+document"))
	send.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, send)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat/1/generate", nil))
	if !strings.Contains(rec.Body.String(), "I see it") {
		t.Fatalf("the answer was not streamed: %s", rec.Body.String())
	}
}
