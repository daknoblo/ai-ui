package llm

import "sync"

// Metrics holds the cumulative token usage since server start plus the usage of
// the most recent chat request. All fields are accessed in a thread-safe way
// through Snapshot/record.
type Metrics struct {
	mu sync.RWMutex

	// Chat (cumulative)
	chatRequests  int64
	chatPromptTok int64
	chatComplTok  int64
	chatTotalTok  int64

	// Most recent chat request
	lastPromptTok int
	lastComplTok  int
	lastTotalTok  int

	// Embeddings (cumulative)
	embedRequests int64
	embedTokens   int64
}

// MetricsSnapshot is a consistent copy of the metrics for display.
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

	TotalTokens int64 // chat + embedding combined
}

// recordChat books the usage of a chat request.
func (m *Metrics) recordChat(u Usage) {
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return // the endpoint did not report any usage
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

// recordEmbedding books the tokens of an embedding request.
func (m *Metrics) recordEmbedding(tokens int) {
	if tokens == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedRequests++
	m.embedTokens += int64(tokens)
}

// Snapshot returns a consistent copy of the current metrics.
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
