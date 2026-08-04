package server

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// checkResult is the outcome of a single readiness check.
type checkResult struct {
	Name   string
	OK     bool
	Detail string
}

// readiness holds the verified state of the required dependencies (storage,
// chat endpoint, embedding endpoint). Uploads are only allowed once storage and
// embeddings have been verified. Every configuration change resets the state so
// that a new check is required.
type readiness struct {
	mu          sync.RWMutex
	storageOK   bool
	chatOK      bool
	embeddingOK bool
	checkedAt   time.Time
}

// invalidate resets all check results.
func (r *readiness) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageOK = false
	r.chatOK = false
	r.embeddingOK = false
	r.checkedAt = time.Time{}
}

// set stores the results of a check run.
func (r *readiness) set(storageOK, chatOK, embeddingOK bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageOK = storageOK
	r.chatOK = chatOK
	r.embeddingOK = embeddingOK
	r.checkedAt = time.Now()
}

// uploadsAllowed reports whether documents may be uploaded. This requires
// storage and the embedding endpoint to be verified.
func (r *readiness) uploadsAllowed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.storageOK && r.embeddingOK
}

// verified reports whether all core components were ready during the last run.
func (r *readiness) verified() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.checkedAt.IsZero() && r.storageOK && r.chatOK && r.embeddingOK
}

// statusSnapshot describes the current connection state for display.
type statusSnapshot struct {
	Checked     bool // has a check run at all?
	StorageOK   bool
	ChatOK      bool
	EmbeddingOK bool
	AllOK       bool
	Uploads     bool
	CheckedAt   time.Time
}

// snapshot returns a consistent copy of the current state.
func (r *readiness) snapshot() statusSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	checked := !r.checkedAt.IsZero()
	return statusSnapshot{
		Checked:     checked,
		StorageOK:   r.storageOK,
		ChatOK:      r.chatOK,
		EmbeddingOK: r.embeddingOK,
		AllOK:       checked && r.storageOK && r.chatOK && r.embeddingOK,
		Uploads:     r.storageOK && r.embeddingOK,
		CheckedAt:   r.checkedAt,
	}
}

// runChecks executes all readiness checks, stores the result and returns the
// individual outcomes for display.
func (s *Server) runChecks(ctx context.Context) []checkResult {
	results := make([]checkResult, 0, 4)

	// 1. Storage reachable & writable.
	storageOK := true
	storageDetail := s.t("check.storage_ready")
	if err := s.store.Ping(ctx); err != nil {
		storageOK = false
		storageDetail = err.Error()
	}
	results = append(results, checkResult{Name: s.t("check.storage"), OK: storageOK, Detail: storageDetail})

	// 2. Chat endpoint reachable.
	chatOK := true
	chatDetail := s.t("check.reachable")
	if err := s.llm.VerifyChat(ctx); err != nil {
		chatOK = false
		chatDetail = err.Error()
	}
	results = append(results, checkResult{Name: s.t("check.chat_endpoint"), OK: chatOK, Detail: chatDetail})

	// 3. Embedding endpoint reachable & returning vectors.
	embeddingOK := true
	embeddingDetail := s.t("check.reachable")
	if err := s.llm.VerifyEmbedding(ctx); err != nil {
		embeddingOK = false
		embeddingDetail = err.Error()
	}
	results = append(results, checkResult{Name: s.t("check.embedding_endpoint"), OK: embeddingOK, Detail: embeddingDetail})

	// 4. Web search (informational only; it never blocks uploads). Checked only
	//    when a provider is configured.
	if s.search.Enabled() {
		searchOK := true
		searchDetail := s.t("check.provider_reachable", s.search.ProviderName())
		if err := s.search.Verify(ctx); err != nil {
			searchOK = false
			searchDetail = err.Error()
		}
		results = append(results, checkResult{Name: s.t("check.web_search"), OK: searchOK, Detail: searchDetail})
	}

	s.ready.set(storageOK, chatOK, embeddingOK)
	return results
}

// Monitor verifies the connection once at start-up and then checks it
// periodically. It only makes sense when the minimum configuration is present;
// otherwise the check is skipped until the app has been configured.
// The function blocks until ctx is canceled (e.g. on shutdown).
func (s *Server) Monitor(ctx context.Context, interval time.Duration) {
	check := func(reason string) {
		// Without the minimum configuration an endpoint check is pointless.
		if !s.cfg.IsConfigured() {
			slog.Info("connection check skipped (not configured)", "reason", reason)
			return
		}
		prev := s.ready.snapshot()
		results := s.runChecks(ctx)
		cur := s.ready.snapshot()

		// Log state changes so outages become visible.
		switch {
		case cur.AllOK && (!prev.Checked || !prev.AllOK):
			slog.Info("connection ready", "reason", reason)
		case !cur.AllOK:
			for _, r := range results {
				if !r.OK {
					slog.Warn("connection check failed", "reason", reason, "check", r.Name, "detail", r.Detail)
				}
			}
		}
	}

	// Check immediately at start-up.
	check("start")

	// interval <= 0 disables the periodic check (start-up check only).
	if interval <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check("periodic")
		}
	}
}
