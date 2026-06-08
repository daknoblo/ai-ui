package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const (
	maxUploadBytes = 25 << 20 // 25 MiB
	retrievalTopK  = 4
	defaultTitle   = "Neuer Chat"
)

// pageData bündelt alle Daten für das Rendern einer Vollseite.
type pageData struct {
	Title       string
	Chats       []storage.Chat
	CurrentChat *storage.Chat
	Messages    []storage.Message
	Documents   []storage.Document
	Configured  bool
}

// buildPageData lädt Chats, Dokumente und ggf. den aktuellen Chat samt Nachrichten.
func (s *Server) buildPageData(ctx context.Context, current *storage.Chat) (pageData, error) {
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return pageData{}, err
	}
	docs, err := s.store.ListDocuments(ctx)
	if err != nil {
		return pageData{}, err
	}

	pd := pageData{
		Title:       "AI UI",
		Chats:       chats,
		CurrentChat: current,
		Documents:   docs,
		Configured:  s.cfg.IsConfigured(),
	}
	if current != nil {
		msgs, err := s.store.ListMessages(ctx, current.ID)
		if err != nil {
			return pageData{}, err
		}
		pd.Messages = msgs
		pd.Title = current.Title
	}
	return pd, nil
}

// handleIndex leitet auf den jüngsten Chat um oder erstellt einen neuen.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		httpError(w, err)
		return
	}
	if len(chats) > 0 {
		http.Redirect(w, r, "/chat/"+strconv.FormatInt(chats[0].ID, 10), http.StatusSeeOther)
		return
	}
	id, err := s.store.CreateChat(ctx, defaultTitle)
	if err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/chat/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleChat rendert die Vollseite eines Chats.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chat, err := s.store.GetChat(ctx, id)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pd, err := s.buildPageData(ctx, &chat)
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, "base", pd)
}

// handleCreateChat legt einen neuen Chat an und leitet per HTMX dorthin um.
func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.CreateChat(r.Context(), defaultTitle)
	if err != nil {
		httpError(w, err)
		return
	}
	redirect(w, r, "/chat/"+strconv.FormatInt(id, 10))
}

// handleDeleteChat entfernt einen Chat.
func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteChat(r.Context(), id); err != nil {
		httpError(w, err)
		return
	}
	redirect(w, r, "/")
}

// handleSend speichert die Nutzernachricht und gibt die Stream-Hülle zurück.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Chat-ID auflösen ("new" erzeugt bei Bedarf einen Chat).
	idParam := chi.URLParam(r, "id")
	var chatID int64
	if idParam == "new" {
		newID, err := s.store.CreateChat(ctx, defaultTitle)
		if err != nil {
			httpError(w, err)
			return
		}
		chatID = newID
	} else {
		parsed, err := strconv.ParseInt(idParam, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		chatID = parsed
	}

	chat, err := s.store.GetChat(ctx, chatID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	message := strings.TrimSpace(r.FormValue("message"))
	if message == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := s.store.AddMessage(ctx, chatID, "user", message); err != nil {
		httpError(w, err)
		return
	}

	// Titel aus erster Nachricht ableiten.
	titleChanged := false
	if chat.Title == defaultTitle {
		newTitle := makeTitle(message)
		if err := s.store.UpdateChatTitle(ctx, chatID, newTitle); err == nil {
			chat.Title = newTitle
			titleChanged = true
		}
	} else {
		_ = s.store.TouchChat(ctx, chatID)
	}

	// Nutzer-Bubble + Stream-Hülle anhängen.
	s.render(w, "message", storage.Message{Role: "user", Content: message})
	s.render(w, "assistant-stream", struct{ ChatID int64 }{ChatID: chatID})
	if titleChanged {
		s.render(w, "title-oob", struct{ Title string }{Title: chat.Title})
	}
}

// handleGenerate streamt die Assistent-Antwort als SSE.
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming nicht unterstützt", http.StatusInternalServerError)
		return
	}

	fail := func(msg string) {
		_ = sse.send("token", renderMarkdownString("⚠ "+msg))
		_ = sse.send("done", "")
	}

	if !s.cfg.IsConfigured() {
		fail("Nicht konfiguriert. Bitte Azure-Endpoint, Deployment, API-Version und AZURE_API_KEY setzen.")
		return
	}

	history, err := s.store.ListMessages(ctx, id)
	if err != nil || len(history) == 0 {
		fail("Keine Nachricht zum Beantworten gefunden.")
		return
	}

	// Letzte Nutzernachricht als Suchanfrage.
	query := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			query = history[i].Content
			break
		}
	}

	messages := s.buildLLMMessages(ctx, s.cfg.Get(), history, query)

	var acc strings.Builder
	streamErr := s.llm.ChatStream(ctx, messages, func(delta string) error {
		acc.WriteString(delta)
		return sse.send("token", renderMarkdownString(acc.String()))
	})

	if streamErr != nil {
		slog.Error("chat-stream", "err", streamErr)
		if acc.Len() == 0 {
			fail("Fehler bei der Anfrage: " + streamErr.Error())
			return
		}
		// Teilantwort vorhanden: kennzeichnen und fortfahren.
		acc.WriteString("\n\n_(Verbindung unterbrochen)_")
		_ = sse.send("token", renderMarkdownString(acc.String()))
	}

	final := acc.String()
	if final != "" {
		if _, err := s.store.AddMessage(context.Background(), id, "assistant", final); err != nil {
			slog.Error("assistant-nachricht speichern", "err", err)
		}
	}
	_ = sse.send("done", "")
}

// buildLLMMessages baut die Nachrichtenliste inkl. RAG-Kontext.
func (s *Server) buildLLMMessages(ctx context.Context, cfg config.Config, history []storage.Message, query string) []llm.Message {
	system := cfg.SystemPrompt

	// Relevante Dokumentabschnitte abrufen (sofern Embedding konfiguriert).
	if cfg.EmbeddingDeployment != "" && query != "" {
		results, err := s.retriever.Retrieve(ctx, query, retrievalTopK)
		if err != nil {
			slog.Warn("retrieval fehlgeschlagen", "err", err)
		} else if len(results) > 0 {
			var sb strings.Builder
			sb.WriteString("\n\nNutze den folgenden Kontext aus hochgeladenen Dokumenten, sofern er für die Frage relevant ist. Wenn er nicht passt, ignoriere ihn.\n\n")
			for i, res := range results {
				fmt.Fprintf(&sb, "[Kontext %d]\n%s\n\n", i+1, res.Text)
			}
			system += sb.String()
		}
	}

	msgs := make([]llm.Message, 0, len(history)+1)
	if system != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: system})
	}
	for _, m := range history {
		msgs = append(msgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	return msgs
}

// handleConfigGet liefert den Einstellungs-Dialog.
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	s.renderConfig(w, false)
}

// handleConfigPost speichert die Konfiguration.
func (s *Server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	cfg := s.cfg.Get()
	cfg.Endpoint = strings.TrimSpace(r.FormValue("endpoint"))
	cfg.ChatDeployment = strings.TrimSpace(r.FormValue("chat_deployment"))
	cfg.APIVersion = strings.TrimSpace(r.FormValue("api_version"))
	cfg.EmbeddingDeployment = strings.TrimSpace(r.FormValue("embedding_deployment"))
	cfg.SystemPrompt = r.FormValue("system_prompt")
	if t, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("temperature")), 64); err == nil {
		cfg.Temperature = t
	}

	if err := s.cfg.Save(cfg); err != nil {
		httpError(w, err)
		return
	}
	s.renderConfig(w, true)
}

// handleUpload nimmt ein Dokument entgegen und verarbeitet es (RAG-Ingestion).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		httpError(w, err)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpError(w, err)
		return
	}
	defer file.Close()

	if !s.cfg.IsConfigured() || s.cfg.Get().EmbeddingDeployment == "" {
		s.renderDocList(w, r, "Embedding nicht konfiguriert – bitte zuerst Einstellungen ausfüllen.")
		return
	}

	data := make([]byte, 0, header.Size)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := file.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}

	mime := header.Header.Get("Content-Type")
	if _, _, err := s.ingestor.Ingest(ctx, header.Filename, mime, data); err != nil {
		slog.Error("ingest", "file", header.Filename, "err", err)
		s.renderDocList(w, r, "Verarbeitung fehlgeschlagen: "+err.Error())
		return
	}

	s.renderDocList(w, r, "")
}

// handleDeleteDocument entfernt ein Dokument.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteDocument(r.Context(), id); err != nil {
		httpError(w, err)
		return
	}
	s.renderDocList(w, r, "")
}

// ---- Render-Helfer ----

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template", "name", name, "err", err)
	}
}

func (s *Server) renderConfig(w http.ResponseWriter, saved bool) {
	data := struct {
		Config config.Config
		HasKey bool
		Saved  bool
	}{
		Config: s.cfg.Get(),
		HasKey: s.cfg.HasAPIKey(),
		Saved:  saved,
	}
	s.render(w, "config", data)
}

func (s *Server) renderDocList(w http.ResponseWriter, r *http.Request, _ string) {
	docs, err := s.store.ListDocuments(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, "doclist", pageData{Documents: docs})
}

// renderMarkdownString rendert Markdown zu einem HTML-String (für SSE).
func renderMarkdownString(src string) string {
	return string(renderMarkdown(src))
}

// ---- sonstige Helfer ----

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// redirect setzt für HTMX-Anfragen den HX-Redirect-Header, sonst klassisch.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func httpError(w http.ResponseWriter, err error) {
	slog.Error("handler", "err", err)
	http.Error(w, "interner fehler", http.StatusInternalServerError)
}

// makeTitle erzeugt einen kurzen Chat-Titel aus der ersten Nachricht.
func makeTitle(msg string) string {
	msg = strings.TrimSpace(strings.ReplaceAll(msg, "\n", " "))
	runes := []rune(msg)
	if len(runes) > 40 {
		return strings.TrimSpace(string(runes[:40])) + "…"
	}
	return msg
}
