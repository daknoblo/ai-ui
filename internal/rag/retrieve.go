package rag

import (
	"context"
	"math"
	"sort"

	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// Result is a hit of the vector search.
type Result struct {
	Text       string
	Score      float32
	DocumentID int64
}

// candidate is a scored chunk before its text has been loaded.
type candidate struct {
	ID         int64
	DocumentID int64
	Score      float32
}

// Retriever runs a brute-force cosine search over the stored chunks.
type Retriever struct {
	store *storage.Store
	llm   *llm.Client
}

// NewRetriever creates a retriever.
func NewRetriever(store *storage.Store, client *llm.Client) *Retriever {
	return &Retriever{store: store, llm: client}
}

// Retrieve returns the topK most relevant chunks for the query, limited to the
// documents of the given chat. It returns an empty list when the chat has no
// documents.
//
// Scoring streams the embeddings and keeps only (id, document, score) per
// chunk. The chunk texts – by far the larger part – are loaded afterwards for
// the handful of selected hits only, which keeps peak memory independent of the
// corpus size.
func (r *Retriever) Retrieve(ctx context.Context, chatID int64, query string, topK int) ([]Result, error) {
	count, err := r.store.CountChunksByChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}

	vecs, err := r.llm.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, nil
	}
	qv := vecs[0]
	qNorm := norm(qv)
	if qNorm == 0 {
		return nil, nil
	}

	scored := make([]candidate, 0, count)
	err = r.store.EachChunkVector(ctx, chatID, func(cv storage.ChunkVector) error {
		if len(cv.Embedding) != len(qv) {
			return nil // skip incompatible dimensions (e.g. after a model change)
		}
		scored = append(scored, candidate{
			ID:         cv.ID,
			DocumentID: cv.DocumentID,
			Score:      cosine(qv, qNorm, cv.Embedding),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(scored) == 0 {
		return nil, nil
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if topK <= 0 {
		topK = 1
	}
	selected := balanceByDocument(scored, topK)

	ids := make([]int64, len(selected))
	for i, c := range selected {
		ids[i] = c.ID
	}
	texts, err := r.store.ChunkTexts(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]Result, 0, len(selected))
	for _, c := range selected {
		text, ok := texts[c.ID]
		if !ok {
			continue // deleted concurrently
		}
		out = append(out, Result{Text: text, Score: c.Score, DocumentID: c.DocumentID})
	}
	return out, nil
}

// balanceByDocument picks results so that every document is represented if
// possible: first the best chunk per document (ordered by relevance), then the
// remaining slots are filled with the globally best chunks. This prevents a
// single document from dominating the whole context.
func balanceByDocument(scored []candidate, topK int) []candidate {
	if len(scored) == 0 {
		return nil
	}

	// Count the documents so that every one of them can get a slot.
	docSeen := make(map[int64]struct{})
	for _, r := range scored {
		docSeen[r.DocumentID] = struct{}{}
	}
	numDocs := len(docSeen)

	// Budget: at least topK, but large enough for one slot per document – with
	// an upper bound so the prompt does not explode.
	const maxChunks = 12
	budget := topK
	if numDocs > budget {
		budget = numDocs
	}
	if budget > maxChunks {
		budget = maxChunks
	}

	out := make([]candidate, 0, budget)
	used := make([]bool, len(scored))

	// Pass 1: best chunk per document (scored is already sorted by score).
	picked := make(map[int64]struct{}, numDocs)
	for i, r := range scored {
		if len(out) >= budget {
			break
		}
		if _, ok := picked[r.DocumentID]; !ok {
			picked[r.DocumentID] = struct{}{}
			out = append(out, r)
			used[i] = true
		}
	}

	// Pass 2: fill the remaining slots with the next best chunks.
	for i, r := range scored {
		if len(out) >= budget {
			break
		}
		if !used[i] {
			out = append(out, r)
			used[i] = true
		}
	}

	// Sort by relevance so the strongest hits come first.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

// norm returns the Euclidean length of a vector.
func norm(v []float32) float64 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	return math.Sqrt(sum)
}

// cosine computes the cosine similarity of two equally long vectors. The norm
// of a is passed in because the query vector is scored against every chunk.
func cosine(a []float32, aNorm float64, b []float32) float32 {
	var dot, nb float64
	for i, av := range a {
		bv := float64(b[i])
		dot += float64(av) * bv
		nb += bv * bv
	}
	if nb == 0 {
		return 0
	}
	return float32(dot / (aNorm * math.Sqrt(nb)))
}
