package rag

import (
	"context"
	"fmt"

	"github.com/daknoblo/ai-ui/internal/docparse"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const (
	defaultChunkSize = 1200 // target size of a chunk in runes
	defaultOverlap   = 200  // overlap between neighboring chunks in runes
	embedBatchSize   = 16   // chunks per embedding request
)

// Ingestor processes uploaded documents: parse -> chunk -> embed -> store.
type Ingestor struct {
	store *storage.Store
	llm   *llm.Client
}

// NewIngestor creates an ingestor.
func NewIngestor(store *storage.Store, client *llm.Client) *Ingestor {
	return &Ingestor{store: store, llm: client}
}

// Ingest processes a single document and returns the created document ID plus
// the number of stored chunks. The document is attached to the given chat.
func (in *Ingestor) Ingest(ctx context.Context, chatID int64, filename, mime string, data []byte) (int64, int, error) {
	text, err := docparse.Extract(filename, mime, data)
	if err != nil {
		return 0, 0, err
	}

	chunks := ChunkText(text, defaultChunkSize, defaultOverlap)
	if len(chunks) == 0 {
		return 0, 0, fmt.Errorf("document contains no usable text")
	}

	// Create the embeddings in batches before the document row is created, so a
	// failing endpoint does not leave an empty document behind.
	embeddings := make([][]float32, 0, len(chunks))
	for start := 0; start < len(chunks); start += embedBatchSize {
		end := min(start+embedBatchSize, len(chunks))
		batch := chunks[start:end]
		vecs, err := in.llm.Embed(ctx, batch)
		if err != nil {
			return 0, 0, fmt.Errorf("embedding failed: %w", err)
		}
		if len(vecs) != len(batch) {
			return 0, 0, fmt.Errorf("unexpected number of embeddings: %d instead of %d", len(vecs), len(batch))
		}
		embeddings = append(embeddings, vecs...)
	}

	docID, err := in.store.CreateDocument(ctx, chatID, filename, mime)
	if err != nil {
		return 0, 0, err
	}

	if err := in.store.AddChunks(ctx, docID, chunks, embeddings); err != nil {
		return docID, 0, err
	}

	return docID, len(chunks), nil
}
