package server

import (
	"context"
	"log/slog"
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

// statusSnapshot beschreibt den aktuellen Verbindungszustand für die Anzeige.
type statusSnapshot struct {
	Checked     bool // wurde überhaupt schon geprüft?
	StorageOK   bool
	ChatOK      bool
	EmbeddingOK bool
	AllOK       bool
	Uploads     bool
	CheckedAt   time.Time
}

// snapshot liefert eine konsistente Kopie des aktuellen Zustands.
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

	// 4. Web-Suche (nur informativ; blockiert keine Uploads). Nur prüfen, wenn
	//    ein Provider konfiguriert ist.
	if s.search.Enabled() {
		searchOK := true
		searchDetail := s.search.ProviderName() + " erreichbar"
		if err := s.search.Verify(ctx); err != nil {
			searchOK = false
			searchDetail = err.Error()
		}
		results = append(results, checkResult{Name: "Web-Suche", OK: searchOK, Detail: searchDetail})
	}

	s.ready.set(storageOK, chatOK, embeddingOK)
	return results
}

// Monitor führt beim Start eine Verifizierung aus und prüft danach periodisch
// die Verbindung. Nur sinnvoll, wenn die Mindestkonfiguration vorhanden ist;
// andernfalls wird die Prüfung übersprungen, bis konfiguriert wurde.
// Die Funktion blockiert bis ctx abgebrochen wird (z.B. beim Shutdown).
func (s *Server) Monitor(ctx context.Context, interval time.Duration) {
	check := func(reason string) {
		// Ohne Mindestkonfiguration macht eine Endpoint-Prüfung keinen Sinn.
		if !s.cfg.IsConfigured() {
			slog.Info("verbindungsprüfung übersprungen (nicht konfiguriert)", "anlass", reason)
			return
		}
		prev := s.ready.snapshot()
		results := s.runChecks(ctx)
		cur := s.ready.snapshot()

		// Zustandsänderungen protokollieren, damit Ausfälle sichtbar werden.
		switch {
		case cur.AllOK && (!prev.Checked || !prev.AllOK):
			slog.Info("verbindung bereit", "anlass", reason)
		case !cur.AllOK:
			for _, r := range results {
				if !r.OK {
					slog.Warn("verbindungsprüfung fehlgeschlagen", "anlass", reason, "check", r.Name, "detail", r.Detail)
				}
			}
		}

		// Modelle automatisch vom Endpoint abrufen, sofern der Chat-Endpoint
		// erreichbar ist und noch keine Liste gepflegt wurde.
		if cur.ChatOK && len(s.cfg.Get().ChatModels) == 0 {
			if n, err := s.refreshModels(ctx); err != nil {
				slog.Warn("modelle automatisch abrufen", "anlass", reason, "err", err)
			} else if n > 0 {
				slog.Info("modelle vom endpoint übernommen", "anzahl", n)
			}
		}
	}

	// Sofort beim Start prüfen.
	check("start")

	// interval <= 0 deaktiviert den periodischen Check (nur Start-Prüfung).
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
			check("periodisch")
		}
	}
}
