package rag

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/documentai"
	"github.com/daknoblo/ai-ui/internal/storage"
)

type fakeAnalysis struct {
	calls int
	pages []documentai.Page
	err   error
}

func (a *fakeAnalysis) Analyze(context.Context, []byte) ([]documentai.Page, error) {
	a.calls++
	return a.pages, a.err
}

func TestDocumentAnalysisIngestion(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateChat(t.Context(), "comparison", "", "")
	if err != nil {
		t.Fatal(err)
	}
	in := NewIngestor(store, nil, Prompts{OCRPage: func(page, total int) string { return "Page reference" }})
	analyzer := &fakeAnalysis{pages: []documentai.Page{{Number: 1, Text: strings.Repeat("Net amount 1234 EUR. ", 200)}, {Number: 2, Text: "Second page 5678 EUR"}}}
	in.SetDocumentAnalyzer(analyzer)
	embedCalls := 0
	embed := func(_ context.Context, texts []string) ([][]float32, error) {
		embedCalls++
		vecs := make([][]float32, len(texts))
		for i, text := range texts {
			if !strings.HasPrefix(text, "\"scan.pdf\"\nPage reference\n") {
				t.Error("chunk lost its document/page reference")
			}
			vecs[i] = []float32{1, 2}
		}
		return vecs, nil
	}
	result, err := in.ingest(t.Context(), id, "scan.pdf", "application/pdf", []byte("%PDF-not-readable-by-legacy-parser"), embed)
	if err != nil || result.AnalysisPages != 2 || result.Chunks < 3 || analyzer.calls != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	beforeCalls := embedCalls
	analyzer.err = errors.New("page analysis failed")
	if _, err := in.ingest(t.Context(), id, "broken.pdf", "application/pdf", []byte("%PDF-test"), embed); err == nil {
		t.Fatal("analysis failure became success")
	}
	docs, err := store.ListDocumentsByChat(t.Context(), id)
	if err != nil || len(docs) != 1 || embedCalls != beforeCalls {
		t.Fatalf("failed document was embedded or stored: docs=%+v err=%v", docs, err)
	}
	_, err = in.ingest(t.Context(), id, "local.txt", "text/plain", []byte("Local text"), func(_ context.Context, texts []string) ([][]float32, error) {
		return [][]float32{{1, 2}}, nil
	})
	if err != nil || analyzer.calls != 2 {
		t.Fatalf("local text reached Azure: %v calls=%d", err, analyzer.calls)
	}
}
