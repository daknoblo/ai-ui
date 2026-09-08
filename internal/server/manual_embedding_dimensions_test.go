package server

import "testing"

func TestManualEmbeddingDimensionDriftFailsWithoutRelabeling(t *testing.T) {
	backend := newManualEmbeddingBackend(t, 2)
	srv, _, chatID := newManualEmbeddingServer(t, backend)
	seedManualEmbedding(t, srv, chatID, true)
	active, _, err := srv.store.ActiveEmbeddingProfile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	backend.overrideDimensions.Store(3)
	if err := srv.verifyActiveEmbedding(t.Context()); err == nil {
		t.Fatal("verification silently accepted changed dimensions")
	}
	if _, err := srv.retriever.Retrieve(t.Context(), chatID, "query", 3); err == nil {
		t.Fatal("retrieval silently skipped incompatible stored vectors")
	}
	if _, err := srv.ingestor.Ingest(t.Context(), chatID, "bad.txt", "text/plain", []byte("new text")); err == nil {
		t.Fatal("upload mixed incompatible vectors into the active index")
	}
	got, known, err := srv.store.ActiveEmbeddingProfile(t.Context())
	if err != nil || !known || got != active {
		t.Fatal("dimension drift changed the active profile")
	}
	documents, chunks, err := srv.store.EmbeddingCounts(t.Context())
	if err != nil || documents != 1 || chunks != 1 {
		t.Fatalf("failed upload mutated the corpus: %d documents, %d chunks, %v", documents, chunks, err)
	}
}
