// Package server contains the HTTP server, its routes and handlers.
package server

import (
	"context"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/logbuf"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
	"github.com/daknoblo/ai-ui/internal/websearch"
	"github.com/daknoblo/ai-ui/web"
)

// Server bundles all dependencies of the HTTP layer.
type Server struct {
	cfg                 *config.Store
	store               *storage.Store
	llm                 *llm.Client
	ingestor            *rag.Ingestor
	retriever           *rag.Retriever
	search              *websearch.Client
	tmpl                *template.Template
	assets              *staticHandler
	ready               *readiness
	logs                *logbuf.Buffer
	ctx                 context.Context
	cancel              context.CancelFunc
	jobs                sync.WaitGroup
	refreshMu           sync.Mutex
	configMu            sync.Mutex
	closing             bool                    // guarded by configMu
	generations         map[int64]generationJob // guarded by configMu
	generationWatchDone chan struct{}
}

// New creates a server and parses the templates.
func New(cfg *config.Store, store *storage.Store, logs *logbuf.Buffer) *Server {
	staticFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		panic("embedded static assets missing: " + err.Error())
	}
	assets, err := newStaticHandler(staticFS)
	if err != nil {
		panic("cannot index static assets: " + err.Error())
	}
	ctx, cancel := context.WithCancel(context.Background())
	if status := cfg.FoundryStatus(); status.Enabled {
		snapshot, ok, err := store.LoadCatalog(ctx, status.ResourceID)
		if err == nil && ok {
			err = cfg.SetCatalog(snapshot)
		}
		if err != nil {
			cfg.SetDiscoveryError(err)
			slog.Warn("load deployment catalog", "err", err)
		}
	}
	if cfg.HasSeparateImageResource() {
		status := cfg.ImageFoundryStatus()
		snapshot, ok, err := store.LoadCatalog(ctx, status.ResourceID)
		if err == nil && ok {
			err = cfg.SetImageCatalog(snapshot)
		}
		if err != nil {
			cfg.SetImageDiscoveryError(err)
			slog.Warn("load image deployment catalog", "err", err)
		}
	}
	client := llm.New(cfg)
	// Persist token usage in the database.
	client.SetUsageRecorder(usageRecorder{store: store})

	// The template helpers read the active language from the configuration on
	// every call. That keeps the language a single global setting instead of a
	// field that would have to be threaded through every template data struct.
	tmpl := template.Must(template.New("").
		Funcs(template.FuncMap{
			"assetURL":       assets.URL,
			"renderMarkdown": renderMarkdown,
			"lang":           cfg.Language,
			"t": func(key string, args ...any) string {
				return i18n.T(cfg.Language(), key, args...)
			},
			"thousands": func(n int64) string {
				return i18n.GroupThousands(cfg.Language(), n)
			},
			// chatTitle substitutes the localized placeholder for chats that
			// have not been named yet.
			"chatTitle": func(title string) string {
				if isUntitled(title) {
					return i18n.T(cfg.Language(), "chat.default_title")
				}
				return title
			},
		}).
		ParseFS(web.TemplatesFS, "templates/*.html"))

	s := &Server{
		ctx: ctx, cancel: cancel,
		cfg:   cfg,
		store: store,
		llm:   client,
		ingestor: rag.NewIngestor(store, client, rag.Prompts{
			OCRSystem: func() string { return i18n.T(cfg.Language(), "prompt.ocr_system") },
			OCRPage: func(page, total int) string {
				return i18n.T(cfg.Language(), "prompt.ocr_page", page, total)
			},
		}),
		retriever:           rag.NewRetriever(store, client),
		search:              websearch.New(cfg),
		tmpl:                tmpl,
		assets:              assets,
		ready:               &readiness{},
		logs:                logs,
		generations:         make(map[int64]generationJob),
		generationWatchDone: make(chan struct{}),
	}
	go s.watchGenerations()
	return s
}

// Close cancels and joins indexing and generation workers before closing SQLite.
func (s *Server) Close() {
	s.cancel()
	// A reindex handler must finish registering its job before Wait starts.
	s.configMu.Lock()
	s.closing = true
	s.configMu.Unlock()
	s.jobs.Wait()
	<-s.generationWatchDone
}

func (s *Server) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// t translates a key into the configured UI language.
func (s *Server) t(key string, args ...any) string {
	return i18n.T(s.cfg.Language(), key, args...)
}

// thousands formats an integer using the separator of the configured language.
func (s *Server) thousands(n int64) string {
	return i18n.GroupThousands(s.cfg.Language(), n)
}

// usageRecorder persists token usage in the data path.
type usageRecorder struct {
	store *storage.Store
}

func (u usageRecorder) RecordUsage(kind, model string, usage llm.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := u.store.RecordUsage(ctx, kind, model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens); err != nil {
		slog.Warn("record token usage", "err", err)
	}
}

// Routes registers all HTTP routes.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.requestContext)
	// middleware.RealIP is deliberately not used: it trusts X-Forwarded-For and
	// friends unconditionally, which allows a client to spoof its address
	// (GHSA-3fxj-6jh8-hvhx). Nothing here reads r.RemoteAddr, and the reverse
	// proxy in front of the app already logs the real client IP.
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(requestLogger)
	// Compression saves a large share of the transferred bytes for HTML and the
	// bundled JavaScript. text/event-stream is deliberately not listed, so SSE
	// responses keep streaming unbuffered.
	r.Use(middleware.Compress(5, "text/html", "text/css", "text/javascript", "application/javascript", "application/json"))

	r.Handle("/static/*", s.assets)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/", s.handleIndex)
	r.Get("/chat/{id}", s.handleChat)
	r.Post("/chats", s.handleCreateChat)
	r.Delete("/chats/{id}", s.handleDeleteChat)
	r.Get("/stats", s.handleStats)
	r.Get("/logs", s.handleLogs)
	r.Get("/logs/tail", s.handleLogTail)
	r.Post("/logs/clear", s.handleLogClear)

	r.Post("/chat/{id}/send", s.handleSend)
	r.Get("/chat/{id}/generate", s.handleGenerate)

	r.Get("/config", s.handleConfigGet)
	r.Post("/config", s.handleConfigPost)
	r.Post("/config/deployments/refresh", s.handleDeploymentRefresh)
	r.Post("/config/embeddings/reindex", s.handleReindex)
	r.Get("/config/embeddings/status", s.handleReindexStatus)
	r.Post("/chat/{id}/model", s.handleSetModel)
	r.Post("/chat/{id}/mode", s.handleSetMode)
	r.Post("/chat/{id}/reasoning", s.handleSetReasoning)
	r.Post("/image/params", s.handleSetImageParams)
	r.Post("/verify", s.handleVerify)
	r.Get("/status", s.handleStatus)

	r.Post("/chat/{id}/documents", s.handleUpload)
	r.Delete("/chat/{cid}/documents/{did}", s.handleDeleteDocument)
	r.Delete("/chat/{cid}/images/{iid}", s.handleDeleteImage)
	r.Get("/images/{id}", s.handleImage)

	return r
}
