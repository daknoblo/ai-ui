package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/documentai"
)

type uploadAnalyzer struct {
	pages []documentai.Page
	err   error
	calls int
}

func (a *uploadAnalyzer) Analyze(context.Context, []byte) ([]documentai.Page, error) {
	a.calls++
	return a.pages, a.err
}

func TestDocumentAnalysisUpload(t *testing.T) {
	srv, handler, vision := ocrServer(t, []string{"gpt-4o"})
	analyzer := &uploadAnalyzer{pages: []documentai.Page{{Number: 1, Text: "Invoice amount 1234 EUR"}, {Number: 2, Text: "Tax 234 EUR"}}}
	srv.ingestor.SetDocumentAnalyzer(analyzer)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, uploadRequest(t, "1", "scan.pdf", "application/pdf", scanPDF(t, 2)))
	if analyzer.calls != 1 || len(*vision) != 0 || !strings.Contains(rec.Body.String(), "Azure Document Intelligence") {
		t.Fatalf("upload did not use Layout exclusively: %s", rec.Body.String())
	}
	docs, err := srv.store.ListDocumentsByChat(t.Context(), 1)
	if err != nil || len(docs) != 1 || docs[0].Chunks != 2 {
		t.Fatalf("stored document=%+v err=%v", docs, err)
	}
	for _, mode := range []string{"failed", "incomplete"} {
		if mode == "failed" {
			analyzer.err = errors.New("Layout failed")
		} else {
			analyzer.err = nil
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, uploadRequest(t, "1", mode+".pdf", "application/pdf", scanPDF(t, 3)))
		docs, err := srv.store.ListDocumentsByChat(t.Context(), 1)
		if err != nil || len(docs) != 1 || !strings.Contains(rec.Body.String(), "Processing failed") {
			t.Fatalf("partial upload accepted: docs=%+v response=%s err=%v", docs, rec.Body.String(), err)
		}
	}
}
