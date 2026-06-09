package rag

import (
	"context"
	"math"
	"sort"

	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// Result ist ein Treffer der Vektorsuche.
type Result struct {
	Text  string
	Score float32
}

// Retriever führt eine Brute-Force-Cosine-Suche über alle gespeicherten Chunks aus.
type Retriever struct {
	store *storage.Store
	llm   *llm.Client
}

// NewRetriever erzeugt einen Retriever.
func NewRetriever(store *storage.Store, client *llm.Client) *Retriever {
	return &Retriever{store: store, llm: client}
}

// Retrieve liefert die topK relevantesten Chunks zur Anfrage, beschränkt auf
// die Dokumente des angegebenen Chats. Sind keine Dokumente vorhanden, wird eine
// leere Liste zurückgegeben.
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

	chunks, err := r.store.ChunksByChat(ctx, chatID)
	if err != nil {
		return nil, err
	}

	scored := make([]Result, 0, len(chunks))
	for _, c := range chunks {
		if len(c.Embedding) != len(qv) {
			continue // inkompatible Dimensionen überspringen
		}
		scored = append(scored, Result{Text: c.Text, Score: cosine(qv, c.Embedding)})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if topK > 0 && len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// cosine berechnet die Kosinus-Ähnlichkeit zweier gleich langer Vektoren.
func cosine(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
