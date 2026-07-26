package storage

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a migrated database in a temporary directory.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// TestChunkRoundTrip covers the split retrieval path: vectors are streamed
// without their text, the texts are loaded separately afterwards.
func TestChunkRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	chatID, err := store.CreateChat(ctx, "test")
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	docID, err := store.CreateDocument(ctx, chatID, "doc.txt", "text/plain")
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	texts := []string{"first section", "second section"}
	embeddings := [][]float32{{1, 0, 0}, {0, 1, 0}}
	if err := store.AddChunks(ctx, docID, texts, embeddings); err != nil {
		t.Fatalf("add chunks: %v", err)
	}

	count, err := store.CountChunksByChat(ctx, chatID)
	if err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if count != len(texts) {
		t.Fatalf("CountChunksByChat = %d, want %d", count, len(texts))
	}

	var (
		ids  []int64
		vecs [][]float32
	)
	err = store.EachChunkVector(ctx, chatID, func(cv ChunkVector) error {
		ids = append(ids, cv.ID)
		// The slice is reused between rows, so it has to be copied here.
		vecs = append(vecs, append([]float32(nil), cv.Embedding...))
		return nil
	})
	if err != nil {
		t.Fatalf("each chunk vector: %v", err)
	}
	if len(vecs) != len(embeddings) {
		t.Fatalf("streamed %d vectors, want %d", len(vecs), len(embeddings))
	}
	for i, want := range embeddings {
		for j, v := range want {
			if vecs[i][j] != v {
				t.Fatalf("vector %d differs at %d: got %v, want %v", i, j, vecs[i][j], v)
			}
		}
	}

	got, err := store.ChunkTexts(ctx, ids)
	if err != nil {
		t.Fatalf("chunk texts: %v", err)
	}
	for i, id := range ids {
		if got[id] != texts[i] {
			t.Errorf("text of chunk %d = %q, want %q", id, got[id], texts[i])
		}
	}
}

// TestAddChunksRejectsMismatch guards the transactional insert against callers
// passing inconsistent slices.
func TestAddChunksRejectsMismatch(t *testing.T) {
	store := newTestStore(t)
	if err := store.AddChunks(t.Context(), 1, []string{"a"}, nil); err == nil {
		t.Fatal("expected an error for mismatched chunk/embedding counts")
	}
}

// TestDeleteChatCascades verifies that documents and chunks disappear with the
// chat they belong to.
func TestDeleteChatCascades(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	chatID, err := store.CreateChat(ctx, "test")
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	docID, err := store.CreateDocument(ctx, chatID, "doc.txt", "text/plain")
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	if err := store.AddChunks(ctx, docID, []string{"text"}, [][]float32{{1}}); err != nil {
		t.Fatalf("add chunks: %v", err)
	}
	if err := store.DeleteChat(ctx, chatID); err != nil {
		t.Fatalf("delete chat: %v", err)
	}

	n, err := store.CountDocuments(ctx)
	if err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if n != 0 {
		t.Errorf("documents remaining after deleting the chat: %d", n)
	}
}
