package llm

import "sync"

// Metrics hält den kumulativen Token-Verbrauch seit Serverstart sowie die
// Nutzung der jeweils letzten Chat-Anfrage. Alle Felder sind thread-sicher
// über Snapshot/record zugänglich.
type Metrics struct {
	mu sync.RWMutex

	// Chat (kumulativ)
	chatRequests  int64
	chatPromptTok int64
	chatComplTok  int64
	chatTotalTok  int64

	// Letzte Chat-Anfrage
	lastPromptTok int
	lastComplTok  int
	lastTotalTok  int

	// Embeddings (kumulativ)
	embedRequests int64
	embedTokens   int64
}

// MetricsSnapshot ist eine konsistente Kopie der Metriken für die Anzeige.
type MetricsSnapshot struct {
	ChatRequests     int64
	ChatPromptTokens int64
	ChatComplTokens  int64
	ChatTotalTokens  int64

	LastPromptTokens int64
	LastComplTokens  int64
	LastTotalTokens  int64

	EmbedRequests int64
	EmbedTokens   int64

	TotalTokens int64 // Chat + Embedding gesamt
}

// recordChat verbucht die Nutzung einer Chat-Anfrage.
func (m *Metrics) recordChat(u Usage) {
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return // Endpoint hat keine Usage gemeldet
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatRequests++
	m.chatPromptTok += int64(u.PromptTokens)
	m.chatComplTok += int64(u.CompletionTokens)
	m.chatTotalTok += int64(u.TotalTokens)
	m.lastPromptTok = u.PromptTokens
	m.lastComplTok = u.CompletionTokens
	m.lastTotalTok = u.TotalTokens
}

// recordEmbedding verbucht die Token einer Embedding-Anfrage.
func (m *Metrics) recordEmbedding(tokens int) {
	if tokens == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedRequests++
	m.embedTokens += int64(tokens)
}

// Snapshot liefert eine konsistente Kopie der aktuellen Metriken.
func (m *Metrics) Snapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return MetricsSnapshot{
		ChatRequests:     m.chatRequests,
		ChatPromptTokens: m.chatPromptTok,
		ChatComplTokens:  m.chatComplTok,
		ChatTotalTokens:  m.chatTotalTok,
		LastPromptTokens: int64(m.lastPromptTok),
		LastComplTokens:  int64(m.lastComplTok),
		LastTotalTokens:  int64(m.lastTotalTok),
		EmbedRequests:    m.embedRequests,
		EmbedTokens:      m.embedTokens,
		TotalTokens:      m.chatTotalTok + m.embedTokens,
	}
}
