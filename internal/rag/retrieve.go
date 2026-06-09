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
	Text       string
	Score      float32
	DocumentID int64
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
		scored = append(scored, Result{Text: c.Text, Score: cosine(qv, c.Embedding), DocumentID: c.DocumentID})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if topK <= 0 {
		topK = 1
	}
	return balanceByDocument(scored, topK), nil
}

// balanceByDocument wählt Ergebnisse so aus, dass möglichst jedes Dokument
// vertreten ist: zuerst der beste Chunk je Dokument (nach Relevanz geordnet),
// danach werden die verbleibenden Plätze mit den global besten Chunks gefüllt.
// So dominiert nicht ein einzelnes Dokument den gesamten Kontext.
func balanceByDocument(scored []Result, topK int) []Result {
	if len(scored) == 0 {
		return nil
	}

	// Anzahl Dokumente bestimmen, um genügend Platz für eine Repräsentation
	// jedes Dokuments zu lassen.
	docSeen := make(map[int64]bool)
	for _, r := range scored {
		docSeen[r.DocumentID] = true
	}
	numDocs := len(docSeen)

	// Budget: mindestens topK, aber so groß, dass jedes Dokument einen Platz
	// bekommt – mit einer Obergrenze, um den Prompt nicht zu sprengen.
	const maxChunks = 12
	budget := topK
	if numDocs > budget {
		budget = numDocs
	}
	if budget > maxChunks {
		budget = maxChunks
	}

	var out []Result
	used := make([]bool, len(scored))

	// 1. Durchgang: bester Chunk je Dokument (scored ist bereits nach Score sortiert).
	picked := make(map[int64]bool)
	for i, r := range scored {
		if len(out) >= budget {
			break
		}
		if !picked[r.DocumentID] {
			picked[r.DocumentID] = true
			out = append(out, r)
			used[i] = true
		}
	}

	// 2. Durchgang: verbleibende Plätze mit den nächstbesten Chunks füllen.
	for i, r := range scored {
		if len(out) >= budget {
			break
		}
		if !used[i] {
			out = append(out, r)
			used[i] = true
		}
	}

	// Final nach Relevanz sortieren, damit die stärksten Treffer zuerst stehen.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
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
