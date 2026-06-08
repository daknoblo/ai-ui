package rag

import (
	"context"
	"fmt"

	"github.com/daknoblo/ai-ui/internal/docparse"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const (
	defaultChunkSize = 1200
	defaultOverlap   = 200
	embedBatchSize   = 16
)

// Ingestor verarbeitet hochgeladene Dokumente: parsen → chunken → embedden → speichern.
type Ingestor struct {
	store *storage.Store
	llm   *llm.Client
}

// NewIngestor erzeugt einen Ingestor.
func NewIngestor(store *storage.Store, client *llm.Client) *Ingestor {
	return &Ingestor{store: store, llm: client}
}

// Ingest verarbeitet ein einzelnes Dokument und liefert die erzeugte Document-ID
// sowie die Anzahl gespeicherter Chunks.
func (in *Ingestor) Ingest(ctx context.Context, filename, mime string, data []byte) (int64, int, error) {
	text, err := docparse.Extract(filename, mime, data)
	if err != nil {
		return 0, 0, err
	}

	chunks := ChunkText(text, defaultChunkSize, defaultOverlap)
	if len(chunks) == 0 {
		return 0, 0, fmt.Errorf("dokument enthält keinen verwertbaren text")
	}

	// Embeddings batchweise erzeugen, bevor das Dokument angelegt wird.
	embeddings := make([][]float32, 0, len(chunks))
	for start := 0; start < len(chunks); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]
		vecs, err := in.llm.Embed(ctx, batch)
		if err != nil {
			return 0, 0, fmt.Errorf("embedding fehlgeschlagen: %w", err)
		}
		if len(vecs) != len(batch) {
			return 0, 0, fmt.Errorf("unerwartete anzahl embeddings: %d statt %d", len(vecs), len(batch))
		}
		embeddings = append(embeddings, vecs...)
	}

	docID, err := in.store.CreateDocument(ctx, filename, mime)
	if err != nil {
		return 0, 0, err
	}

	for i, chunk := range chunks {
		if err := in.store.AddChunk(ctx, docID, i, chunk, embeddings[i]); err != nil {
			return docID, i, err
		}
	}

	return docID, len(chunks), nil
}
