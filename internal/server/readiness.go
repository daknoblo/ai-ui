package server

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
)

// checkResult is the outcome of a single readiness check.
type checkResult struct {
	Key     string
	Name    string
	OK      bool
	Detail  string
	Target  string
	Skipped bool
	Info    bool
}

func (r *readiness) invalidateImageChecks(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	r.results = slices.Clone(r.results)
	for i := range r.results {
		if r.results[i].Key == "image-api" {
			r.results[i].OK = false
			r.results[i].Skipped = true
			r.results[i].Detail = detail
		}
	}
}

func (r checkResult) State() string {
	if r.Skipped {
		return "skipped"
	}
	if !r.OK {
		return "err"
	}
	if r.Info {
		return "info"
	}
	return "ok"
}

// readiness holds the verified state of the required dependencies (storage,
// chat endpoint, embedding endpoint). Uploads are only allowed once storage and
// embeddings have been verified. Every configuration change resets the state so
// that a new check is required.
type readiness struct {
	mu                sync.RWMutex
	storageOK         bool
	chatOK            bool
	embeddingOK       bool
	embeddingOptional bool
	generation        uint64
	checkedAt         time.Time
	// results of the last full check, so the settings dialog can show it
	// without probing every deployment again.
	results   []checkResult
	resultsAt time.Time
}

// invalidate resets all check results.
func (r *readiness) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	r.storageOK = false
	r.chatOK = false
	r.embeddingOK = false
	r.checkedAt = time.Time{}
	r.results = nil
	r.resultsAt = time.Time{}
}

func (r *readiness) epoch() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

func (r *readiness) finish(generation uint64, storageOK, chatOK, embeddingOK, optional bool, results []checkResult, recordResults bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != generation {
		return false
	}
	r.storageOK, r.chatOK, r.embeddingOK = storageOK, chatOK, embeddingOK
	r.embeddingOptional = optional
	r.checkedAt = time.Now()
	if recordResults {
		r.results, r.resultsAt = results, r.checkedAt
	}
	return true
}

// lastResults returns the outcome of the last full check and when it ran.
func (r *readiness) lastResults() ([]checkResult, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.results, r.resultsAt
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
	return !r.checkedAt.IsZero() && r.storageOK && r.chatOK && (r.embeddingOK || r.embeddingOptional)
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
		AllOK:       checked && r.storageOK && r.chatOK && (r.embeddingOK || r.embeddingOptional),
		Uploads:     r.storageOK && r.embeddingOK,
		CheckedAt:   r.checkedAt,
	}
}

// runChecks executes all readiness checks, stores the result and returns the
// individual outcomes for display. With deep every deployment of AZURE_MODELS is
// probed as well, which is too expensive for the periodic check but exactly what
// the button in the settings dialog is for.
func (s *Server) runChecks(ctx context.Context, deep bool) []checkResult {
	results := make([]checkResult, 0, 4)
	generation := s.ready.epoch()
	cfg := s.cfg.Get()
	embeddingOptional := cfg.Foundry && cfg.EmbeddingDeployment == ""

	// 1. Storage reachable & writable.
	storageOK := true
	storageDetail := s.t("check.storage_ready")
	if err := s.store.Ping(ctx); err != nil {
		storageOK = false
		storageDetail = err.Error()
	}
	results = append(results, checkResult{Name: s.t("check.storage"), OK: storageOK, Detail: storageDetail})

	results = append(results, s.inventoryChecks()...)

	// 2. Check saved selections, not unsaved values in the settings form.
	chatOK := true
	chatDetail := s.t("check.reachable")
	chatSkipped := cfg.ChatDeployment == ""
	if chatSkipped {
		chatOK, chatDetail = false, s.t("check.select_model")
	} else if err := s.llm.VerifyChat(ctx); err != nil {
		chatOK = false
		chatDetail = err.Error()
	}
	results = append(results, checkResult{Name: s.t("check.chat_endpoint"), Target: cfg.ChatDeployment,
		OK: chatOK, Detail: chatDetail, Skipped: chatSkipped})

	// 3. Embedding endpoint reachable & returning vectors.
	embeddingOK := true
	embeddingDetail := s.t("check.reachable")
	embeddingSkipped := cfg.EmbeddingDeployment == ""
	embeddingTarget := cfg.EmbeddingDeployment
	if embeddingSkipped {
		embeddingOK = false
		embeddingDetail = s.t("check.optional_not_configured")
	} else if err := s.verifyActiveEmbedding(ctx); err != nil {
		embeddingOK = false
		embeddingDetail = err.Error()
	}
	if !embeddingSkipped {
		active, known, err := s.store.ActiveEmbeddingProfile(ctx)
		if err != nil {
			embeddingOK, embeddingDetail = false, err.Error()
		} else if known {
			embeddingTarget = active.Deployment
			target, targetErr := s.llm.ConfiguredEmbeddingProfile()
			if embeddingOK && targetErr == nil && !active.SameIdentity(target) {
				embeddingDetail = s.t("check.embedding_active", target.Deployment+" — "+target.Endpoint)
			}
		}
	}
	results = append(results, checkResult{Name: s.t("check.embedding_endpoint"), Target: embeddingTarget,
		OK: embeddingOK, Detail: embeddingDetail, Skipped: embeddingSkipped})

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

	// 5. Every selectable deployment (informational as well): a typo in
	//    AZURE_MODELS would otherwise only surface when that model is picked.
	if deep && chatOK && !cfg.Foundry {
		for _, model := range s.cfg.Get().ChatModels {
			detail := s.t("check.reachable")
			ok := true
			if err := s.llm.VerifyDeployment(ctx, model); err != nil {
				ok = false
				detail = err.Error()
			}
			results = append(results, checkResult{Name: s.t("check.deployment", model), OK: ok, Detail: detail})
		}
	}

	if cfg.Foundry {
		vision := checkResult{Name: s.t("check.vision_endpoint"), Target: cfg.VisionDeployment, Info: true}
		if vision.Target == "" {
			if _, err := s.cfg.ResolveDeployment(foundry.Vision, cfg.ChatDeployment); err == nil {
				vision.Target = cfg.ChatDeployment
			}
		}
		switch {
		case vision.Target == "":
			vision.Skipped, vision.Detail = true, s.t("check.optional_not_configured")
		case !deep:
			vision.Skipped, vision.Detail = true, s.t("check.manual_only")
		default:
			_, err := s.cfg.ResolveDeployment(foundry.Vision, vision.Target)
			if err == nil {
				if vision.Target == cfg.ChatDeployment {
					vision.OK = chatOK
					vision.Detail = chatDetail
				} else if err := s.llm.VerifyDeployment(ctx, vision.Target); err != nil {
					vision.Detail = err.Error()
				} else {
					vision.OK = true
				}
			} else {
				vision.Detail = err.Error()
			}
			if vision.OK {
				vision.Detail = s.t("check.vision_metadata_only")
			}
		}
		results = append(results, vision)
	}

	// An incomplete image request must never be presented as a successful generation.
	if cfg.ImageDeployment == "" {
		if cfg.Foundry {
			results = append(results, checkResult{Key: "image-api", Name: s.t("check.image_endpoint"),
				Skipped: true, Detail: s.t("check.optional_not_configured")})
		}
	} else if !deep {
		if cfg.Foundry {
			results = append(results, checkResult{Key: "image-api", Name: s.t("check.image_endpoint"), Target: cfg.ImageDeployment,
				Skipped: true, Detail: s.t("check.manual_only")})
		}
	} else {
		models := cfg.ImageModels
		if cfg.Foundry {
			models = []string{cfg.ImageDeployment}
		}
		for _, model := range models {
			detail := s.t("check.image_probe_only")
			ok := true
			if err := s.llm.VerifyImage(ctx, model); err != nil {
				ok = false
				detail = err.Error()
			}
			results = append(results, checkResult{Key: "image-api", Name: s.t("check.image_endpoint"), Target: model,
				OK: ok, Detail: detail, Info: true})
		}
	}

	if !s.ready.finish(generation, storageOK, chatOK, embeddingOK, embeddingOptional, results, true) {
		results = append(results, checkResult{Name: s.t("config.title"), Detail: s.t("foundry.check_changed")})
	}
	return results
}

// Inventory is metadata, so settings can show its latest refresh without
// repeating the billable inference checks or invalidating chat/embedding health.
func (s *Server) inventoryChecks() []checkResult {
	status := s.cfg.FoundryStatus()
	if !status.Enabled {
		return nil
	}
	catalogCheck := func(key, name string, state config.FoundryStatus) checkResult {
		result := checkResult{Key: key, Name: name, Info: true}
		switch {
		case state.IdentityError != "":
			result.Detail = state.IdentityError
		case state.RefreshError != "":
			result.Detail = state.RefreshError
		case state.Catalog.RefreshedAt.IsZero():
			result.Skipped, result.Detail = true, s.t("check.refresh_first")
		default:
			result.OK, result.Detail = true, s.t("check.catalog_cached", formatCheckTime(state.Catalog.RefreshedAt))
		}
		return result
	}
	results := []checkResult{catalogCheck("foundry-inventory", s.t("check.foundry_inventory"), status)}
	if s.cfg.HasSeparateImageResource() {
		return append(results, catalogCheck("foundry-image-inventory", s.t("check.image_inventory"), s.cfg.ImageFoundryStatus()))
	}
	images := checkResult{Key: "foundry-image-catalog", Name: s.t("check.image_catalog"), Info: true}
	switch {
	case status.Catalog.ImageCatalogError != "":
		images.Detail = status.Catalog.ImageCatalogError
	case !status.Catalog.ImageCatalogChecked:
		images.Skipped, images.Detail = true, s.t("check.refresh_first")
	default:
		images.OK, images.Detail = true, s.t("check.image_catalog_listed")
	}
	return append(results, images)
}

func (s *Server) currentInventoryResults(results []checkResult) []checkResult {
	results = slices.DeleteFunc(slices.Clone(results), func(result checkResult) bool {
		return result.Key == "foundry-inventory" || result.Key == "foundry-image-inventory" ||
			result.Key == "foundry-image-catalog"
	})
	return slices.Insert(results, min(1, len(results)), s.inventoryChecks()...)
}

// Monitor verifies the connection once at start-up and then checks it
// periodically. It only makes sense when the minimum configuration is present;
// otherwise the check is skipped until the app has been configured.
// The function blocks until ctx is canceled (e.g. on shutdown).
func (s *Server) Monitor(ctx context.Context, interval time.Duration) {
	stopCancel := context.AfterFunc(ctx, s.cancel)
	defer stopCancel()
	if s.cfg.FoundryStatus().Enabled {
		// Discovery must also work before any role has been selected.
		if err := s.refreshFoundry(ctx); err != nil {
			slog.Warn("initial deployment discovery failed", "err", err)
		}
	}
	check := func(reason string, deep bool) {
		// Without the minimum configuration an endpoint check is pointless.
		if !s.cfg.IsConfigured() && !s.cfg.FoundryStatus().Enabled {
			slog.Info("connection check skipped (not configured)", "reason", reason)
			return
		}
		prev := s.ready.snapshot()
		results := s.runChecks(ctx, deep)
		cur := s.ready.snapshot()

		// Log state changes so outages become visible.
		switch {
		case cur.AllOK && (!prev.Checked || !prev.AllOK):
			slog.Info("connection ready", "reason", reason)
		case !cur.AllOK:
			for _, r := range results {
				if !r.OK && !r.Skipped {
					slog.Warn("connection check failed", "reason", reason, "check", r.Name, "detail", r.Detail)
				}
			}
		}
	}

	// Check immediately at start-up, including every deployment, so the settings
	// dialog can show a complete result without probing again.
	check("start", true)

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
			check("periodic", false)
		}
	}
}
