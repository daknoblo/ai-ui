package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/llm"
)

// scanPDF builds a PDF whose pages carry nothing but a JPEG - a scan.
func scanPDF(t *testing.T, pages int) []byte {
	t.Helper()

	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 2048)...)
	jpeg = append(jpeg, 0xFF, 0xD9)

	var objs [][]byte
	objs = append(objs, []byte("<< /Type /Catalog /Pages 2 0 R >>"))
	kids := make([]string, 0, pages)
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i*3))
	}
	objs = append(objs, fmt.Appendf(nil, "<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), pages))

	for i := range pages {
		content, xobj := 4+i*3, 5+i*3
		objs = append(objs, fmt.Appendf(nil,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>", xobj, content))
		draw := []byte("q 612 0 0 792 0 0 cm /Im0 Do Q")
		objs = append(objs, fmt.Appendf(nil, "<< /Length %d >>\nstream\n%s\nendstream", len(draw), draw))
		objs = append(objs, fmt.Appendf(nil,
			"<< /Type /XObject /Subtype /Image /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream",
			len(jpeg), jpeg))
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, obj := range objs {
		offsets[i+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, xref)
	return pdf.Bytes()
}

// ocrServer wires a server whose endpoint transcribes any image request and
// answers embedding requests with a fixed vector.
func ocrServer(t *testing.T, models []string) (*Server, http.Handler, *[]map[string]any) {
	t.Helper()
	var chats []map[string]any

	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.Header().Set("Content-Type", "application/json")
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var sb strings.Builder
			sb.WriteString(`{"data":[`)
			for i := range req.Input {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"index":%d,"embedding":[0.5,0.5]}`, i)
			}
			sb.WriteString(`],"usage":{"total_tokens":1}}`)
			_, _ = w.Write([]byte(sb.String()))
			return
		}

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		chats = append(chats, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":" +
			"{\"content\":\"Invoice total 1234 EUR\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(azure.Close)

	srv, handler := newConfiguredServer(t, i18n.EN,
		config.Keys{API: "key"}, config.Overrides{ChatModels: models},
		func(cfg *config.Config) {
			cfg.Endpoint = azure.URL + "/openai/v1"
			cfg.ChatDeployment = "model-router"
			cfg.EmbeddingDeployment = "text-embedding-3-large"
		})
	// Uploads are gated on a verified connection.
	srv.ready.set(true, true, true)

	if _, err := srv.store.CreateChat(t.Context(), untitled, "", llm.ReasoningAuto); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return srv, handler, &chats
}

// TestScanIsReadByTheVisionModel is the end-to-end proof for the OCR path: a
// PDF without a text layer is uploaded, transcribed page by page and stored as
// a searchable document.
func TestScanIsReadByTheVisionModel(t *testing.T) {
	srv, handler, chats := ocrServer(t, []string{"gpt-4o"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", "invoice.pdf", "application/pdf", scanPDF(t, 2)))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// One request per page, each of them carrying that page as an image.
	if len(*chats) != 2 {
		t.Fatalf("got %d transcription requests, want one per page", len(*chats))
	}
	for i, body := range *chats {
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 {
			t.Fatalf("request %d has %d messages, want system + page", i+1, len(messages))
		}
		user, _ := messages[1].(map[string]any)
		parts, ok := user["content"].([]any)
		if !ok {
			t.Fatalf("request %d carries no page image: %v", i+1, user["content"])
		}
		var images int
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if part["type"] == "image_url" {
				images++
			}
		}
		if images != 1 {
			t.Errorf("request %d carries %d images, want exactly the one page", i+1, images)
		}
	}

	// The transcript has to end up in the document store, not just in a log.
	docs, err := srv.store.ListDocumentsByChat(t.Context(), 1)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	if len(docs) != 1 || docs[0].Name != "invoice.pdf" {
		t.Fatalf("the scan was not stored: %+v", docs)
	}
	if docs[0].Chunks == 0 {
		t.Error("the transcript produced no searchable chunks")
	}
	if !strings.Contains(rec.Body.String(), "vision model") {
		t.Errorf("the upload notice does not mention the transcription: %s", rec.Body.String())
	}
}

// TestScanWithoutVisionModelFails checks the honest failure: with no deployment
// that can see, the upload has to say so instead of storing an empty document.
func TestScanWithoutVisionModelFails(t *testing.T) {
	srv, handler, chats := ocrServer(t, []string{"gpt-3.5-turbo"})
	cfg := srv.cfg.Get()
	cfg.ChatModel = "gpt-3.5-turbo"
	cfg.ChatDeployment = "gpt-3.5-turbo"
	if err := srv.cfg.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", "scan.pdf", "application/pdf", scanPDF(t, 1)))

	if len(*chats) != 0 {
		t.Errorf("a blind deployment must not be sent images, got %d requests", len(*chats))
	}
	docs, err := srv.store.ListDocumentsByChat(t.Context(), 1)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("an unreadable scan must not be stored: %+v", docs)
	}
	if !strings.Contains(rec.Body.String(), "no text layer") {
		t.Errorf("the notice does not explain the failure: %s", rec.Body.String())
	}
}

// TestTextPDFSkipsTranscription guards the cost: a PDF that has a text layer
// must be parsed, never sent to the model page by page.
func TestTextPDFSkipsTranscription(t *testing.T) {
	_, handler, chats := ocrServer(t, []string{"gpt-4o"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", "notes.txt", "text/plain",
		[]byte("A plain document with real text in it.")))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}
	if len(*chats) != 0 {
		t.Errorf("a readable document triggered %d model requests, want none", len(*chats))
	}
}

// TestUploadRejectsUnverifiedConnection keeps the existing gate in place.
func TestUploadRejectsUnverifiedConnection(t *testing.T) {
	srv, handler, _ := ocrServer(t, []string{"gpt-4o"})
	srv.ready.invalidate()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", "notes.txt", "text/plain", []byte("text")))

	docs, err := srv.store.ListDocumentsByChat(t.Context(), 1)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("an unverified connection must not ingest: %+v", docs)
	}
}
