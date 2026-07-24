package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const (
	maxUploadBytes      = 25 << 20  // 25 MiB pro Datei
	maxTotalUploadBytes = 150 << 20 // 150 MiB pro Anfrage (Mehrfach-Upload)
	retrievalTopK       = 8
	defaultTitle        = "Neuer Chat"
)

// pageData bündelt alle Daten für das Rendern einer Vollseite.
type pageData struct {
	Title         string
	Chats         []storage.Chat
	CurrentChat   *storage.Chat
	Messages      []storage.Message
	Documents     []storage.Document
	Configured    bool
	ChatID        int64
	Notice        string
	NoticeErr     bool
	Models        []string
	CurrentModel  string
	UploadsReady  bool
	SearchEnabled bool
	StatusBadge   statusBadge
}

// buildPageData lädt Chats, Dokumente und ggf. den aktuellen Chat samt Nachrichten.
func (s *Server) buildPageData(ctx context.Context, current *storage.Chat) (pageData, error) {
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return pageData{}, err
	}
	cfg := s.cfg.Get()

	pd := pageData{
		Title:         "AI UI",
		Chats:         chats,
		CurrentChat:   current,
		Configured:    s.cfg.IsConfigured(),
		Models:        cfg.ChatModels,
		CurrentModel:  cfg.ChatModel,
		UploadsReady:  s.ready.uploadsAllowed(),
		SearchEnabled: s.search.Enabled(),
		StatusBadge:   s.statusData(),
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

// handleIndex öffnet immer einen frischen neuen Chat und räumt dabei verwaiste
// leere Chats auf.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Verwaiste leere Chats entfernen, bevor ein neuer angelegt wird.
	if _, err := s.store.DeleteEmptyChats(ctx, 0); err != nil {
		slog.Warn("leere chats aufräumen", "err", err)
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
	// Beim Öffnen eines Chats verwaiste leere Chats entfernen (außer diesem).
	if _, err := s.store.DeleteEmptyChats(ctx, id); err != nil {
		slog.Warn("leere chats aufräumen", "err", err)
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
	ctx := r.Context()
	// Verwaiste leere Chats entfernen, bevor ein neuer angelegt wird.
	if _, err := s.store.DeleteEmptyChats(ctx, 0); err != nil {
		slog.Warn("leere chats aufräumen", "err", err)
	}
	id, err := s.store.CreateChat(ctx, defaultTitle)
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

// handleStats zeigt die persistente Token-Nutzungsstatistik.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	summary, err := s.store.UsageSummaryTotals(ctx)
	if err != nil {
		httpError(w, err)
		return
	}
	days, err := s.store.UsageByDay(ctx, 30)
	if err != nil {
		httpError(w, err)
		return
	}
	models, err := s.store.UsageByModel(ctx)
	if err != nil {
		httpError(w, err)
		return
	}
	data := struct {
		Title   string
		Summary storage.UsageSummary
		Days    []storage.UsageDay
		Models  []storage.UsageModel
	}{
		Title:   "Statistik",
		Summary: summary,
		Days:    days,
		Models:  models,
	}
	s.render(w, "stats", data)
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

	// Web-Suche nur berücksichtigen, wenn angefordert UND konfiguriert.
	web := r.FormValue("web") == "1" && s.search.Enabled()

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
	s.render(w, "assistant-stream", struct {
		ChatID int64
		Web    bool
	}{ChatID: chatID, Web: web})
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

	// Web-Suche: erzwungen per Toggle (?web=1) oder automatisch per Tool-Calling.
	cfg := s.cfg.Get()
	forceWeb := r.URL.Query().Get("web") == "1" && s.search.Enabled()
	autoWeb := cfg.SearchAuto && s.search.Enabled() && !forceWeb

	messages := s.buildLLMMessages(ctx, id, cfg, history, query, forceWeb)

	var acc strings.Builder
	onDelta := func(delta string) error {
		acc.WriteString(delta)
		return sse.send("token", renderMarkdownString(acc.String()))
	}

	var (
		result    llm.ChatResult
		streamErr error
	)
	if autoWeb {
		result, streamErr = s.streamWithSearch(ctx, sse, messages, onDelta)
		// Fallback ohne Tools, falls der Router kein Tool-Calling unterstützt.
		if streamErr != nil && acc.Len() == 0 {
			slog.Warn("tool-calling fehlgeschlagen, fallback ohne tools", "err", streamErr)
			result, streamErr = s.llm.ChatStream(ctx, messages, onDelta)
		}
	} else {
		result, streamErr = s.llm.ChatStream(ctx, messages, onDelta)
	}

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

	// Tatsächlich verwendetes Modell anzeigen (vom Router gemeldet).
	if result.Model != "" {
		_ = sse.send("model", s.renderString("model-tag", result.Model))
	}

	// Token-Nutzung dieser Antwort als Fußzeile der Nachricht ausgeben.
	if result.Usage.TotalTokens > 0 {
		_ = sse.send("usage", fmt.Sprintf("%s Tokens · %s Eingabe / %s Antwort",
			groupThousands(int64(result.Usage.TotalTokens)),
			groupThousands(int64(result.Usage.PromptTokens)),
			groupThousands(int64(result.Usage.CompletionTokens))))
	}

	// Nach der ersten Antwort einen prägnanten Chat-Titel erzeugen.
	if final != "" {
		s.maybeGenerateTitle(context.Background(), sse, id)
	}
	_ = sse.send("done", "")
}

// maybeGenerateTitle erzeugt nach dem ersten Austausch einen kurzen, prägnanten
// Chat-Titel aus dem Inhalt und aktualisiert Header und Seitenleiste per SSE.
func (s *Server) maybeGenerateTitle(ctx context.Context, sse *sseWriter, chatID int64) {
	msgs, err := s.store.ListMessages(ctx, chatID)
	if err != nil || len(msgs) != 2 { // nur beim ersten Austausch (1 Frage + 1 Antwort)
		return
	}
	if !s.cfg.IsConfigured() {
		return
	}

	var userMsg, assistantMsg string
	for _, m := range msgs {
		switch m.Role {
		case "user":
			userMsg = m.Content
		case "assistant":
			assistantMsg = m.Content
		}
	}
	if userMsg == "" {
		return
	}

	prompt := fmt.Sprintf(
		"Erstelle einen sehr kurzen, prägnanten Titel (höchstens 6 Wörter, keine Anführungszeichen, kein abschließendes Satzzeichen) für diese Unterhaltung.\n\nFrage: %s\n\nAntwort: %s",
		truncateRunes(userMsg, 800), truncateRunes(assistantMsg, 800))

	titleMessages := []llm.Message{
		{Role: "system", Content: "Du erstellst extrem kurze, prägnante Titel für Chat-Unterhaltungen."},
		{Role: "user", Content: prompt},
	}

	var sb strings.Builder
	if _, err := s.llm.ChatStream(ctx, titleMessages, func(delta string) error {
		sb.WriteString(delta)
		return nil
	}); err != nil {
		slog.Warn("titel erzeugen", "err", err)
		return
	}

	title := cleanTitle(sb.String())
	if title == "" {
		return
	}
	if err := s.store.UpdateChatTitle(ctx, chatID, title); err != nil {
		slog.Warn("titel speichern", "err", err)
		return
	}

	// Header und Seitenleiste live aktualisieren.
	chats, _ := s.store.ListChats(ctx)
	chat, _ := s.store.GetChat(ctx, chatID)
	data := struct {
		Title       string
		Chats       []storage.Chat
		CurrentChat *storage.Chat
	}{Title: title, Chats: chats, CurrentChat: &chat}
	_ = sse.send("title", s.renderString("title-update", data))
}

// cleanTitle bereinigt einen vom Modell erzeugten Titel.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'„“”`")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > 60 {
		s = strings.TrimSpace(string(runes[:60]))
	}
	return s
}

// truncateRunes kürzt einen Text auf höchstens n Runen.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// buildLLMMessages baut die Nachrichtenliste inkl. RAG- und optionalem Web-Kontext
// (RAG ist auf den Chat beschränkt).
func (s *Server) buildLLMMessages(ctx context.Context, chatID int64, cfg config.Config, history []storage.Message, query string, web bool) []llm.Message {
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

	// Aktuelle Web-Ergebnisse einbeziehen, falls angefordert.
	if web && query != "" {
		results, err := s.search.Search(ctx, query)
		if err != nil {
			slog.Warn("websuche fehlgeschlagen", "err", err)
		} else if len(results) > 0 {
			var sb strings.Builder
			sb.WriteString("\n\nNutze die folgenden aktuellen Web-Ergebnisse, sofern sie für die Frage relevant sind. Zitiere die Quellen mit ihrer URL.\n\n")
			for i, res := range results {
				fmt.Fprintf(&sb, "[Web %d] %s\nQuelle: %s\n%s\n\n", i+1, res.Title, res.URL, res.Content)
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

// maxToolIterations begrenzt die Runden im Tool-Loop, um Endlosschleifen zu vermeiden.
const maxToolIterations = 4

// webSearchTool definiert das dem Modell angebotene Websuche-Werkzeug.
func webSearchTool() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "web_search",
			Description: "Durchsucht das Web nach aktuellen Informationen. Nutze dieses Werkzeug, wenn die Frage aktuelle Ereignisse, Nachrichten, Preise, Zahlen oder Fakten betrifft, die sich seit deinem Trainingsstand geändert haben könnten.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Die Suchanfrage in natürlicher Sprache"}
				},
				"required": ["query"]
			}`),
		},
	}
}

// streamWithSearch führt den Tool-Loop aus: Das Modell kann das web_search-Tool
// selbst aufrufen; die Ergebnisse werden zurückgespeist, bis eine finale Antwort
// (ohne Tool-Aufruf) gestreamt wird.
func (s *Server) streamWithSearch(ctx context.Context, sse *sseWriter, messages []llm.Message, onDelta func(string) error) (llm.ChatResult, error) {
	tools := []llm.Tool{webSearchTool()}
	var final llm.ChatResult

	for i := 0; i < maxToolIterations; i++ {
		turn, err := s.llm.ChatStreamWithTools(ctx, messages, tools, onDelta)
		if err != nil {
			return final, err
		}
		if turn.Model != "" {
			final.Model = turn.Model
		}
		final.Usage.PromptTokens += turn.Usage.PromptTokens
		final.Usage.CompletionTokens += turn.Usage.CompletionTokens
		final.Usage.TotalTokens += turn.Usage.TotalTokens

		// Keine Tool-Aufrufe → finale Antwort wurde bereits gestreamt.
		if len(turn.ToolCalls) == 0 {
			return final, nil
		}

		// Assistenten-Nachricht mit den angeforderten Tool-Aufrufen anhängen.
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   turn.Content,
			ToolCalls: turn.ToolCalls,
		})
		// Jeden Tool-Aufruf ausführen und das Ergebnis zurückspeisen.
		for _, tc := range turn.ToolCalls {
			resultText := s.executeToolCall(ctx, sse, tc)
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    resultText,
			})
		}
	}

	// Maximale Rundenzahl erreicht: letzte Antwort ohne Tools erzwingen.
	turn, err := s.llm.ChatStream(ctx, messages, onDelta)
	if err != nil {
		return final, err
	}
	if turn.Model != "" {
		final.Model = turn.Model
	}
	final.Usage.PromptTokens += turn.Usage.PromptTokens
	final.Usage.CompletionTokens += turn.Usage.CompletionTokens
	final.Usage.TotalTokens += turn.Usage.TotalTokens
	return final, nil
}

// executeToolCall führt einen Tool-Aufruf aus und liefert das Ergebnis als Text
// für das Modell. Aktuell wird nur "web_search" unterstützt.
func (s *Server) executeToolCall(ctx context.Context, sse *sseWriter, tc llm.ToolCall) string {
	if tc.Function.Name != "web_search" {
		return "Unbekanntes Werkzeug: " + tc.Function.Name
	}

	var args struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "Leere Suchanfrage."
	}

	// Status in der UI anzeigen.
	_ = sse.send("tool", s.renderString("tool-status", query))

	results, err := s.search.Search(ctx, query)
	if err != nil {
		slog.Warn("websuche (tool) fehlgeschlagen", "query", query, "err", err)
		return "Websuche fehlgeschlagen: " + err.Error()
	}
	if len(results) == 0 {
		return "Keine Web-Ergebnisse gefunden."
	}

	var sb strings.Builder
	sb.WriteString("Web-Ergebnisse (zitiere relevante Quellen mit ihrer URL):\n\n")
	for i, res := range results {
		fmt.Fprintf(&sb, "[%d] %s\nURL: %s\n%s\n\n", i+1, res.Title, res.URL, res.Content)
	}
	return sb.String()
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
	locks := s.cfg.Locks()
	// Per Umgebungsvariable gesperrte Endpoint-Felder werden im Formular
	// deaktiviert (und daher nicht übertragen); sie dürfen nicht überschrieben
	// werden. Save() schützt sie zusätzlich, hier vermeiden wir das Leeren.
	if !locks.Endpoint {
		cfg.Endpoint = strings.TrimSpace(r.FormValue("endpoint"))
	}
	if !locks.ChatDeployment {
		cfg.ChatDeployment = strings.TrimSpace(r.FormValue("chat_deployment"))
	}
	if !locks.ChatModels {
		cfg.ChatModels = parseModels(r.FormValue("chat_models"))
	}
	if !locks.APIVersion {
		cfg.APIVersion = strings.TrimSpace(r.FormValue("api_version"))
	}
	if !locks.EmbeddingEndpoint {
		cfg.EmbeddingEndpoint = strings.TrimSpace(r.FormValue("embedding_endpoint"))
	}
	if !locks.EmbeddingDeployment {
		cfg.EmbeddingDeployment = strings.TrimSpace(r.FormValue("embedding_deployment"))
	}
	if !locks.EmbeddingAPIVersion {
		cfg.EmbeddingAPIVersion = strings.TrimSpace(r.FormValue("embedding_api_version"))
	}
	cfg.SearchProvider = strings.ToLower(strings.TrimSpace(r.FormValue("search_provider")))
	cfg.SearchEndpoint = strings.TrimSpace(r.FormValue("search_endpoint"))
	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("search_max_results"))); err == nil && n > 0 {
		cfg.SearchMaxResults = n
	}
	cfg.SearchAuto = r.FormValue("search_auto") == "on"
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
		StatusBadge    statusBadge
	}{
		Results:        results,
		Verified:       s.ready.verified(),
		UploadsAllowed: s.ready.uploadsAllowed(),
		StatusBadge:    s.statusData(),
	}
	s.render(w, "verify-results", data)
}

// handleStatus liefert den Verbindungs-Badge für die Seitenleiste. Wird von der
// UI periodisch gepollt, damit Verbindungsausfälle sichtbar werden.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.render(w, "status-badge", s.statusData())
}

// statusData bereitet die Daten für den Status-Badge auf.
func (s *Server) statusData() statusBadge {
	snap := s.ready.snapshot()
	docs, _ := s.store.CountDocuments(context.Background())
	m := s.llm.Metrics()
	b := statusBadge{
		Configured: s.cfg.IsConfigured(),
		Checked:    snap.Checked,
		AllOK:      snap.AllOK,
		Uploads:    snap.Uploads,
		DiskBytes:  s.store.DiskUsage(),
		DocCount:   docs,
		Metrics:    m,
		HasUsage:   m.TotalTokens > 0,
	}
	b.DiskHuman = humanBytes(b.DiskBytes)
	switch {
	case !b.Configured:
		b.Label = "Nicht konfiguriert"
		b.Level = "warn"
	case !snap.Checked:
		b.Label = "Prüfe…"
		b.Level = "warn"
	case snap.AllOK:
		b.Label = "Verbunden"
		b.Level = "ok"
	case !snap.StorageOK:
		b.Label = "Speicher-Fehler"
		b.Level = "err"
	case !snap.ChatOK && !snap.EmbeddingOK:
		b.Label = "Endpoints nicht erreichbar"
		b.Level = "err"
	case !snap.ChatOK:
		b.Label = "Chat-Endpoint offline"
		b.Level = "err"
	default:
		b.Label = "Embedding-Endpoint offline"
		b.Level = "err"
	}
	return b
}

// statusBadge sind die Anzeigedaten des Verbindungsstatus.
type statusBadge struct {
	Configured bool
	Checked    bool
	AllOK      bool
	Uploads    bool
	DiskBytes  int64
	DiskHuman  string
	DocCount   int
	Metrics    llm.MetricsSnapshot
	HasUsage   bool
	Label      string
	Level      string // ok | warn | err
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

// refreshModels fragt die verfügbaren Modelle vom Endpoint ab und speichert sie
// in der Konfiguration. Liefert die Anzahl gefundener Modelle.
func (s *Server) refreshModels(ctx context.Context) (int, error) {
	models, err := s.llm.ListModels(ctx)
	if err != nil {
		return 0, err
	}
	cfg := s.cfg.Get()
	cfg.ChatModels = models
	// Erzwungenes Modell verwerfen, falls es nicht mehr angeboten wird.
	if cfg.ChatModel != "" && !containsString(models, cfg.ChatModel) {
		cfg.ChatModel = ""
	}
	if err := s.cfg.Save(cfg); err != nil {
		return 0, err
	}
	return len(models), nil
}

// handleRefreshModels fragt die Modelle vom Endpoint ab (Button im Konfig-Dialog)
// und rendert den Konfig-Dialog mit dem Ergebnis neu.
func (s *Server) handleRefreshModels(w http.ResponseWriter, r *http.Request) {
	// Ist die Modell-Liste per Umgebungsvariable festgelegt, ist sie gesperrt.
	if s.cfg.Locks().ChatModels {
		s.renderConfigNotice(w, "Modelle sind über die Umgebungsvariable AZURE_CHAT_MODELS festgelegt und können hier nicht abgerufen werden.", true)
		return
	}
	n, err := s.refreshModels(r.Context())
	if err != nil {
		slog.Warn("modelle abrufen", "err", err)
		s.renderConfigNotice(w, "Modelle konnten nicht abgerufen werden: "+err.Error(), true)
		return
	}
	s.renderConfigNotice(w, fmt.Sprintf("%d bereitgestellte Modelle übernommen.", n), false)
}

// parseModels zerlegt eine durch Zeilen oder Kommas getrennte Liste in
// bereinigte, eindeutige Modellnamen.
func parseModels(raw string) []string {
	return config.ParseModelList(raw)
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

	r.Body = http.MaxBytesReader(w, r.Body, maxTotalUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		httpError(w, err)
		return
	}

	ecfg := s.cfg.Get()
	if ecfg.EmbeddingDeployment == "" || ecfg.EmbeddingHost() == "" || !s.cfg.HasEmbeddingAPIKey() {
		s.renderDocList(w, r, chatID, "Embedding nicht konfiguriert – bitte zuerst Einstellungen ausfüllen.", true)
		return
	}

	var headers []*multipart.FileHeader
	if r.MultipartForm != nil {
		headers = r.MultipartForm.File["file"]
	}
	if len(headers) == 0 {
		s.renderDocList(w, r, chatID, "Keine Datei empfangen.", true)
		return
	}

	var (
		added    int
		failures []string
	)
	for _, header := range headers {
		if header.Size > maxUploadBytes {
			failures = append(failures, fmt.Sprintf("„%s“ (zu groß)", header.Filename))
			continue
		}
		data, err := readMultipartFile(header)
		if err != nil {
			slog.Error("upload lesen", "file", header.Filename, "err", err)
			failures = append(failures, fmt.Sprintf("„%s“ (Lesefehler)", header.Filename))
			continue
		}
		mime := header.Header.Get("Content-Type")
		if _, _, err := s.ingestor.Ingest(ctx, chatID, header.Filename, mime, data); err != nil {
			slog.Error("ingest", "file", header.Filename, "err", err)
			failures = append(failures, fmt.Sprintf("„%s“ (%s)", header.Filename, err.Error()))
			continue
		}
		added++
	}

	notice, isErr := uploadSummary(added, failures)
	s.renderDocList(w, r, chatID, notice, isErr)
}

// readMultipartFile liest den gesamten Inhalt einer hochgeladenen Datei.
func readMultipartFile(header *multipart.FileHeader) ([]byte, error) {
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	data := make([]byte, 0, header.Size)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, rerr
		}
	}
	return data, nil
}

// uploadSummary baut die Statusmeldung für einen (Mehrfach-)Upload.
func uploadSummary(added int, failures []string) (string, bool) {
	switch {
	case added > 0 && len(failures) == 0:
		if added == 1 {
			return "1 Dokument hinzugefügt.", false
		}
		return fmt.Sprintf("%d Dokumente hinzugefügt.", added), false
	case added > 0 && len(failures) > 0:
		return fmt.Sprintf("%d hinzugefügt. Fehlgeschlagen: %s", added, strings.Join(failures, ", ")), true
	default:
		return "Verarbeitung fehlgeschlagen: " + strings.Join(failures, ", "), true
	}
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

// renderString rendert ein Template in einen String (für SSE-Events).
func (s *Server) renderString(name string, data any) string {
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("template", "name", name, "err", err)
		return ""
	}
	return buf.String()
}

func (s *Server) renderConfig(w http.ResponseWriter, saved bool) {
	s.renderConfigData(w, saved, "", false)
}

// renderConfigNotice rendert den Konfig-Dialog mit einer Statusmeldung.
func (s *Server) renderConfigNotice(w http.ResponseWriter, notice string, isErr bool) {
	s.renderConfigData(w, false, notice, isErr)
}

func (s *Server) renderConfigData(w http.ResponseWriter, saved bool, notice string, noticeErr bool) {
	data := struct {
		Config             config.Config
		Locks              config.Locks
		HasKey             bool
		HasEmbeddingKey    bool
		HasOwnEmbeddingKey bool
		HasSearchKey       bool
		SearchEnabled      bool
		Saved              bool
		Verified           bool
		UploadsAllowed     bool
		Notice             string
		NoticeErr          bool
	}{
		Config:             s.cfg.Get(),
		Locks:              s.cfg.Locks(),
		HasKey:             s.cfg.HasAPIKey(),
		HasEmbeddingKey:    s.cfg.HasEmbeddingAPIKey(),
		HasOwnEmbeddingKey: s.cfg.HasOwnEmbeddingAPIKey(),
		HasSearchKey:       s.cfg.HasSearchAPIKey(),
		SearchEnabled:      s.search.Enabled(),
		Saved:              saved,
		Verified:           s.ready.verified(),
		UploadsAllowed:     s.ready.uploadsAllowed(),
		Notice:             notice,
		NoticeErr:          noticeErr,
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

// humanBytes formatiert eine Byte-Größe als lesbare Zeichenkette (z.B. "2.4 MB").
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// groupThousands formatiert eine Ganzzahl mit deutschen Tausender-Trennpunkten
// (z.B. 1234567 -> "1.234.567").
func groupThousands(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	// Von rechts in Dreiergruppen mit "." trennen.
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s[i : i+3])
	}
	out := b.String()
	if neg {
		return "-" + out
	}
	return out
}
