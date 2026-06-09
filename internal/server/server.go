// Package server enthält den HTTP-Server, die Routen und Handler.
package server

import (
	"html/template"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
	"github.com/daknoblo/ai-ui/internal/websearch"
	"github.com/daknoblo/ai-ui/web"
)

// Server bündelt alle Abhängigkeiten der HTTP-Schicht.
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

// New erzeugt einen Server und parst die Templates.
func New(cfg *config.Store, store *storage.Store) *Server {
	client := llm.New(cfg)

	tmpl := template.Must(template.New("").
		Funcs(template.FuncMap{"renderMarkdown": renderMarkdown}).
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

// Routes registriert alle HTTP-Routen.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	// Statische Assets aus dem eingebetteten Dateisystem.
	staticFS, _ := fs.Sub(web.StaticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	r.Get("/", s.handleIndex)
	r.Get("/chat/{id}", s.handleChat)
	r.Post("/chats", s.handleCreateChat)
	r.Delete("/chats/{id}", s.handleDeleteChat)

	r.Post("/chat/{id}/send", s.handleSend)
	r.Get("/chat/{id}/generate", s.handleGenerate)

	r.Get("/config", s.handleConfigGet)
	r.Post("/config", s.handleConfigPost)
	r.Post("/model", s.handleSetModel)
	r.Post("/verify", s.handleVerify)
	r.Get("/status", s.handleStatus)

	r.Post("/chat/{id}/documents", s.handleUpload)
	r.Delete("/chat/{cid}/documents/{did}", s.handleDeleteDocument)

	return r
}
