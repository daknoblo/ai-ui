// Package server contains the HTTP server, its routes and handlers.
package server

import (
	"context"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
	"github.com/daknoblo/ai-ui/internal/websearch"
	"github.com/daknoblo/ai-ui/web"
)

// Server bundles all dependencies of the HTTP layer.
type Server struct {
	cfg       *config.Store
	store     *storage.Store
	llm       *llm.Client
	ingestor  *rag.Ingestor
	retriever *rag.Retriever
	search    *websearch.Client
	tmpl      *template.Template
	ready     *readiness
}

// New creates a server and parses the templates.
func New(cfg *config.Store, store *storage.Store) *Server {
	client := llm.New(cfg)
	// Persist token usage in the database.
	client.SetUsageRecorder(usageRecorder{store: store})

	// The template helpers read the active language from the configuration on
	// every call. That keeps the language a single global setting instead of a
	// field that would have to be threaded through every template data struct.
	tmpl := template.Must(template.New("").
		Funcs(template.FuncMap{
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

	return &Server{
		cfg:       cfg,
		store:     store,
		llm:       client,
		ingestor:  rag.NewIngestor(store, client),
		retriever: rag.NewRetriever(store, client),
		search:    websearch.New(cfg),
		tmpl:      tmpl,
		ready:     &readiness{},
	}
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
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(requestLogger)
	// Compression saves a large share of the transferred bytes for HTML and the
	// bundled JavaScript. text/event-stream is deliberately not listed, so SSE
	// responses keep streaming unbuffered.
	r.Use(middleware.Compress(5, "text/html", "text/css", "text/javascript", "application/javascript", "application/json"))

	// Static assets from the embedded file system, served with content based
	// ETags so browsers can revalidate cheaply (see newStaticHandler).
	staticFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		panic("embedded static assets missing: " + err.Error())
	}
	staticHandler, err := newStaticHandler(staticFS)
	if err != nil {
		panic("cannot index static assets: " + err.Error())
	}
	r.Handle("/static/*", staticHandler)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/", s.handleIndex)
	r.Get("/chat/{id}", s.handleChat)
	r.Post("/chats", s.handleCreateChat)
	r.Delete("/chats/{id}", s.handleDeleteChat)
	r.Get("/stats", s.handleStats)

	r.Post("/chat/{id}/send", s.handleSend)
	r.Get("/chat/{id}/generate", s.handleGenerate)

	r.Get("/config", s.handleConfigGet)
	r.Post("/config", s.handleConfigPost)
	r.Post("/model", s.handleSetModel)
	r.Post("/models/refresh", s.handleRefreshModels)
	r.Post("/verify", s.handleVerify)
	r.Get("/status", s.handleStatus)

	r.Post("/chat/{id}/documents", s.handleUpload)
	r.Delete("/chat/{cid}/documents/{did}", s.handleDeleteDocument)

	return r
}
