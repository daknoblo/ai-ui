package server

import (
	"context"
	"sync"
	"time"
)

// checkResult ist das Ergebnis einer einzelnen Bereitschaftsprüfung.
type checkResult struct {
	Name   string
	OK     bool
	Detail string
}

// readiness hält den verifizierten Zustand der benötigten Abhängigkeiten
// (Storage, Chat-Endpoint, Embedding-Endpoint). Uploads sind erst erlaubt, wenn
// Storage und Embedding verifiziert wurden. Jede Konfigurationsänderung setzt
// den Zustand zurück, sodass erneut geprüft werden muss.
type readiness struct {
	mu          sync.RWMutex
	storageOK   bool
	chatOK      bool
	embeddingOK bool
	checkedAt   time.Time
}

// invalidate setzt alle Prüfergebnisse zurück.
func (r *readiness) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageOK = false
	r.chatOK = false
	r.embeddingOK = false
	r.checkedAt = time.Time{}
}

// set übernimmt die Ergebnisse eines Prüflaufs.
func (r *readiness) set(storageOK, chatOK, embeddingOK bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageOK = storageOK
	r.chatOK = chatOK
	r.embeddingOK = embeddingOK
	r.checkedAt = time.Now()
}

// uploadsAllowed gibt an, ob Dokumente hochgeladen werden dürfen.
// Voraussetzung: Storage und Embedding-Endpoint sind verifiziert.
func (r *readiness) uploadsAllowed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.storageOK && r.embeddingOK
}

// verified gibt an, ob seit dem letzten Prüflauf alle Kernkomponenten bereit sind.
func (r *readiness) verified() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.checkedAt.IsZero() && r.storageOK && r.chatOK && r.embeddingOK
}

// runChecks führt alle Bereitschaftsprüfungen aus, speichert das Ergebnis und
// liefert die Einzelresultate für die Anzeige.
func (s *Server) runChecks(ctx context.Context) []checkResult {
	results := make([]checkResult, 0, 3)

	// 1. Storage erreichbar & schreibbar.
	storageOK := true
	storageDetail := "bereit"
	if err := s.store.Ping(ctx); err != nil {
		storageOK = false
		storageDetail = err.Error()
	}
	results = append(results, checkResult{Name: "Speicher (Datenpfad)", OK: storageOK, Detail: storageDetail})

	// 2. Chat-Endpoint erreichbar.
	chatOK := true
	chatDetail := "erreichbar"
	if err := s.llm.VerifyChat(ctx); err != nil {
		chatOK = false
		chatDetail = err.Error()
	}
	results = append(results, checkResult{Name: "Chat-Endpoint", OK: chatOK, Detail: chatDetail})

	// 3. Embedding-Endpoint erreichbar & liefert Vektoren.
	embeddingOK := true
	embeddingDetail := "erreichbar"
	if err := s.llm.VerifyEmbedding(ctx); err != nil {
		embeddingOK = false
		embeddingDetail = err.Error()
	}
	results = append(results, checkResult{Name: "Embedding-Endpoint", OK: embeddingOK, Detail: embeddingDetail})

	s.ready.set(storageOK, chatOK, embeddingOK)
	return results
}
