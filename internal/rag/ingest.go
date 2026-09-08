package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/daknoblo/ai-ui/internal/docparse"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const (
	defaultChunkSize = 1200 // target size of a chunk in runes
	defaultOverlap   = 200  // overlap between neighboring chunks in runes
	embedBatchSize   = 16   // chunks per embedding request
)

// Prompts supplies the localized wording the ingestion needs when it has to
// read a scan. It lives here rather than in the llm package so the text follows
// the UI language like every other user visible string.
type Prompts struct {
	// OCRSystem instructs the model to transcribe rather than summarize.
	OCRSystem func() string
	// OCRPage labels a single page of a transcription.
	OCRPage func(page, total int) string
}

// IngestResult describes what an ingestion produced.
type IngestResult struct {
	DocumentID int64
	Chunks     int
	// OCRPages is greater than zero when the document had no text layer and was
	// read off its page images instead.
	OCRPages int
}

// Ingestor processes uploaded documents: parse -> chunk -> embed -> store.
type Ingestor struct {
	store   *storage.Store
	llm     *llm.Client
	prompts Prompts
}

// NewIngestor creates an ingestor.
func NewIngestor(store *storage.Store, client *llm.Client, prompts Prompts) *Ingestor {
	return &Ingestor{store: store, llm: client, prompts: prompts}
}

// Ingest processes a single document and returns what was stored. The document
// is attached to the given chat.
func (in *Ingestor) Ingest(ctx context.Context, chatID int64, filename, mime string, data []byte) (IngestResult, error) {
	var result IngestResult
	err := in.store.WithCorpusMutation(ctx, func() error {
		return in.store.WithEmbeddingProfile(ctx, func(profile storage.EmbeddingProfile, known bool) error {
			if !known || profile.Dimensions <= 0 {
				return storage.ErrReindexRequired
			}
			embed := func(ctx context.Context, inputs []string) ([][]float32, error) {
				return in.llm.EmbedProfile(ctx, profile, inputs)
			}
			var err error
			result, err = in.ingest(ctx, chatID, filename, mime, data, embed)
			return err
		})
	})
	return result, err
}

func (in *Ingestor) ingest(ctx context.Context, chatID int64, filename, mime string, data []byte,
	embed func(context.Context, []string) ([][]float32, error),
) (IngestResult, error) {
	var res IngestResult

	text, err := docparse.Extract(filename, mime, data)
	// A PDF without a text layer is a scan. Its pages are images, so the model
	// that reads images can supply the text the parser could not find.
	if errors.Is(err, docparse.ErrNeedsOCR) {
		text, res.OCRPages, err = in.transcribe(ctx, filename, data)
	}
	if err != nil {
		return res, err
	}

	chunks := ChunkText(text, defaultChunkSize, defaultOverlap)
	if len(chunks) == 0 {
		return res, fmt.Errorf("document contains no usable text")
	}

	// Create the embeddings in batches before the document row is created, so a
	// failing endpoint does not leave an empty document behind.
	embeddings := make([][]float32, 0, len(chunks))
	for start := 0; start < len(chunks); start += embedBatchSize {
		end := min(start+embedBatchSize, len(chunks))
		batch := chunks[start:end]
		vecs, err := embed(ctx, batch)
		if err != nil {
			return res, fmt.Errorf("embedding failed: %w", err)
		}
		if len(vecs) != len(batch) {
			return res, fmt.Errorf("unexpected number of embeddings: %d instead of %d", len(vecs), len(batch))
		}
		embeddings = append(embeddings, vecs...)
	}

	docID, err := in.store.CreateDocument(ctx, chatID, filename, mime)
	if err != nil {
		return res, err
	}
	res.DocumentID = docID

	if err := in.store.AddChunks(ctx, docID, chunks, embeddings); err != nil {
		return res, err
	}
	res.Chunks = len(chunks)
	return res, nil
}

// transcribe reads a scanned document off its page images.
func (in *Ingestor) transcribe(ctx context.Context, filename string, data []byte) (string, int, error) {
	if !in.llm.CanTranscribe() {
		return "", 0, fmt.Errorf("pdf has no text layer and no vision capable deployment is configured to read it")
	}

	pages := docparse.ScanImages(data)
	if len(pages) == 0 {
		return "", 0, fmt.Errorf("pdf has no text layer and no readable page images")
	}

	images := make([]llm.ImageContent, 0, len(pages))
	for _, page := range pages {
		images = append(images, llm.ImageContent{MIME: page.MIME, Data: page.Data})
	}

	slog.Info("reading a scan with the vision model", "file", filename, "pages", len(images))
	res, err := in.llm.Transcribe(ctx, in.prompts.OCRSystem(), in.prompts.OCRPage, images)
	if err != nil {
		return "", 0, err
	}
	slog.Info("scan transcribed", "file", filename,
		"pages", res.Pages, "of", len(images), "model", res.Model, "tokens", res.Usage.TotalTokens)
	return res.Text, res.Pages, nil
}
