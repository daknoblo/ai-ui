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
	Title        string
	Chats        []storage.Chat
	CurrentChat  *storage.Chat
	Messages     []storage.Message
	Documents    []storage.Document
	Configured   bool
	ChatID       int64
	Notice       string
	NoticeErr    bool
	Models       []string
	CurrentModel string
	UploadsReady bool
}

// buildPageData lädt Chats, Dokumente und ggf. den aktuellen Chat samt Nachrichten.
func (s *Server) buildPageData(ctx context.Context, current *storage.Chat) (pageData, error) {
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return pageData{}, err
	}
	cfg := s.cfg.Get()

	pd := pageData{
		Title:        "AI UI",
		Chats:        chats,
		CurrentChat:  current,
		Configured:   s.cfg.IsConfigured(),
		Models:       cfg.ChatModels,
		CurrentModel: cfg.ChatModel,
		UploadsReady: s.ready.uploadsAllowed(),
	}
	if current != nil {
		msgs, err := s.store.ListMessages(ctx, current.ID)
		if err != nil {
			return pageData{}, err
		}
		docs, err := s.store.ListDocumentsByChat(ctx, current.ID)
		if err != nil {
			return pageData{}, err
		}
		pd.Messages = msgs
		pd.Documents = docs
		pd.Title = current.Title
		pd.ChatID = current.ID
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

	messages := s.buildLLMMessages(ctx, id, s.cfg.Get(), history, query)

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

// buildLLMMessages baut die Nachrichtenliste inkl. RAG-Kontext (auf den Chat beschränkt).
func (s *Server) buildLLMMessages(ctx context.Context, chatID int64, cfg config.Config, history []storage.Message, query string) []llm.Message {
	system := cfg.SystemPrompt

	// Relevante Dokumentabschnitte abrufen (sofern Embedding konfiguriert).
	if cfg.EmbeddingDeployment != "" && query != "" {
		results, err := s.retriever.Retrieve(ctx, chatID, query, retrievalTopK)
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
	cfg.ChatModels = parseModels(r.FormValue("chat_models"))
	cfg.APIVersion = strings.TrimSpace(r.FormValue("api_version"))
	cfg.EmbeddingEndpoint = strings.TrimSpace(r.FormValue("embedding_endpoint"))
	cfg.EmbeddingDeployment = strings.TrimSpace(r.FormValue("embedding_deployment"))
	cfg.EmbeddingAPIVersion = strings.TrimSpace(r.FormValue("embedding_api_version"))
	cfg.SystemPrompt = r.FormValue("system_prompt")
	if t, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("temperature")), 64); err == nil {
		cfg.Temperature = t
	}

	// Aktuell erzwungenes Modell verwerfen, falls es nicht mehr in der Liste steht.
	if cfg.ChatModel != "" && !containsString(cfg.ChatModels, cfg.ChatModel) {
		cfg.ChatModel = ""
	}

	if err := s.cfg.Save(cfg); err != nil {
		httpError(w, err)
		return
	}
	// Konfiguration geändert: Verifizierung muss erneut erfolgen.
	s.ready.invalidate()
	s.renderConfig(w, true)
}

// handleVerify führt alle Bereitschaftsprüfungen aus und liefert das Ergebnis.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	results := s.runChecks(r.Context())
	data := struct {
		Results        []checkResult
		Verified       bool
		UploadsAllowed bool
	}{
		Results:        results,
		Verified:       s.ready.verified(),
		UploadsAllowed: s.ready.uploadsAllowed(),
	}
	s.render(w, "verify-results", data)
}

// handleSetModel übernimmt die Modellauswahl aus dem Header-Menü. Die Auswahl
// ist global und bleibt damit beim Wechsel zwischen Chats erhalten.
func (s *Server) handleSetModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if err := s.cfg.SetChatModel(model); err != nil {
		slog.Warn("modellauswahl abgelehnt", "model", model, "err", err)
		http.Error(w, "ungültiges modell", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseModels zerlegt eine durch Zeilen oder Kommas getrennte Liste in
// bereinigte, eindeutige Modellnamen.
func parseModels(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	seen := make(map[string]struct{}, len(fields))
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// containsString prüft, ob s in list enthalten ist.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// handleUpload nimmt ein Dokument entgegen und verarbeitet es (RAG-Ingestion).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	chatID, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetChat(ctx, chatID); err != nil {
		http.NotFound(w, r)
		return
	}

	// Uploads sind erst erlaubt, wenn Storage und Embedding-Endpoint verifiziert
	// wurden. So gelangen keine Dokumente in die Pipeline, bevor die benötigten
	// Komponenten nachweislich bereit sind.
	if !s.ready.uploadsAllowed() {
		s.renderDocList(w, r, chatID,
			"Upload gesperrt – bitte zuerst in den Einstellungen die Verbindung testen (Speicher & Embedding-Endpoint müssen bereit sein).", true)
		return
	}

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

	ecfg := s.cfg.Get()
	if ecfg.EmbeddingDeployment == "" || ecfg.EmbeddingHost() == "" || !s.cfg.HasEmbeddingAPIKey() {
		s.renderDocList(w, r, chatID, "Embedding nicht konfiguriert – bitte zuerst Einstellungen ausfüllen.", true)
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
	_, n, err := s.ingestor.Ingest(ctx, chatID, header.Filename, mime, data)
	if err != nil {
		slog.Error("ingest", "file", header.Filename, "err", err)
		s.renderDocList(w, r, chatID, "Verarbeitung fehlgeschlagen: "+err.Error(), true)
		return
	}
	s.renderDocList(w, r, chatID, fmt.Sprintf("„%s“ hinzugefügt (%d Abschnitte).", header.Filename, n), false)
}

// handleDeleteDocument entfernt ein Dokument aus einem Chat.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	chatID, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	docID, err := strconv.ParseInt(chi.URLParam(r, "did"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteDocument(r.Context(), docID); err != nil {
		httpError(w, err)
		return
	}
	s.renderDocList(w, r, chatID, "", false)
}

// ---- Render-Helfer ----

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template", "name", name, "err", err)
	}
}

func (s *Server) renderConfig(w http.ResponseWriter, saved bool) {
	data := struct {
		Config             config.Config
		HasKey             bool
		HasEmbeddingKey    bool
		HasOwnEmbeddingKey bool
		Saved              bool
		Verified           bool
		UploadsAllowed     bool
	}{
		Config:             s.cfg.Get(),
		HasKey:             s.cfg.HasAPIKey(),
		HasEmbeddingKey:    s.cfg.HasEmbeddingAPIKey(),
		HasOwnEmbeddingKey: s.cfg.HasOwnEmbeddingAPIKey(),
		Saved:              saved,
		Verified:           s.ready.verified(),
		UploadsAllowed:     s.ready.uploadsAllowed(),
	}
	s.render(w, "config", data)
}

func (s *Server) renderDocList(w http.ResponseWriter, r *http.Request, chatID int64, notice string, isErr bool) {
	docs, err := s.store.ListDocumentsByChat(r.Context(), chatID)
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, "doclist", pageData{ChatID: chatID, Documents: docs, Notice: notice, NoticeErr: isErr})
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
