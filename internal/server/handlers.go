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
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
	"github.com/daknoblo/ai-ui/internal/websearch"
)

const (
	maxUploadBytes      = 25 << 20 // 25 MiB per file
	maxTotalUploadBytes = 150 << 20
	// multipartMemoryBytes is how much of a multipart form is buffered in RAM;
	// anything above it spills to a temporary file. Keeping this well below the
	// per-file limit bounds memory usage when several files arrive at once.
	multipartMemoryBytes = 8 << 20 // 8 MiB
	retrievalTopK        = 8
)

// untitled is the title stored for a chat that has not been named yet. It is
// intentionally empty so the displayed placeholder can follow the UI language.
const untitled = ""

// legacyDefaultTitles are the placeholder titles written by older versions,
// which stored a translated string instead of an empty title.
var legacyDefaultTitles = []string{"Neuer Chat", "New chat"}

// isUntitled reports whether a chat still carries the placeholder title.
func isUntitled(title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return true
	}
	for _, t := range legacyDefaultTitles {
		if title == t {
			return true
		}
	}
	return false
}

// pageData bundles all data needed to render a full page.
type pageData struct {
	Title          string
	Chats          []storage.Chat
	CurrentChat    *storage.Chat
	Messages       []storage.Message
	Documents      []storage.Document
	Configured     bool
	ChatID         int64
	Notice         string
	NoticeErr      bool
	Models         []string
	CurrentModel   string
	UploadsReady   bool
	SearchEnabled  bool
	ImageEnabled   bool
	ImageSize      string
	ImageQuality   string
	ImageFormat    string
	ImageSizes     []string
	ImageQualities []string
	ImageFormats   []string
	StatusBadge    statusBadge
}

// buildPageData loads chats, documents and – if given – the current chat with
// its messages.
func (s *Server) buildPageData(ctx context.Context, current *storage.Chat) (pageData, error) {
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return pageData{}, err
	}
	cfg := s.cfg.Get()

	pd := pageData{
		Title:          "AI UI",
		Chats:          chats,
		CurrentChat:    current,
		Configured:     s.cfg.IsConfigured(),
		Models:         cfg.ChatModels,
		CurrentModel:   cfg.ChatModel,
		UploadsReady:   s.ready.uploadsAllowed(),
		SearchEnabled:  s.search.Enabled(),
		ImageEnabled:   s.cfg.ImagesConfigured(),
		ImageSize:      cfg.ImageSize,
		ImageQuality:   cfg.ImageQuality,
		ImageFormat:    cfg.ImageFormat,
		ImageSizes:     imageSizes,
		ImageQualities: imageQualities,
		ImageFormats:   imageFormats,
		StatusBadge:    s.statusData(),
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
		pd.Title = s.chatTitle(current.Title)
		pd.ChatID = current.ID
	}
	return pd, nil
}

// chatTitle returns the displayable title of a chat, substituting the localized
// placeholder for chats that have not been named yet.
func (s *Server) chatTitle(title string) string {
	if isUntitled(title) {
		return s.t("chat.default_title")
	}
	return title
}

// handleIndex always opens a fresh chat and cleans up orphaned empty ones.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Remove orphaned empty chats before creating a new one.
	if _, err := s.store.DeleteEmptyChats(ctx, 0); err != nil {
		slog.Warn("clean up empty chats", "err", err)
	}
	id, err := s.store.CreateChat(ctx, untitled)
	if err != nil {
		s.httpError(w, err)
		return
	}
	http.Redirect(w, r, "/chat/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleChat renders the full page of a chat.
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
	// Remove orphaned empty chats when opening a chat (except this one).
	if _, err := s.store.DeleteEmptyChats(ctx, id); err != nil {
		slog.Warn("clean up empty chats", "err", err)
	}
	pd, err := s.buildPageData(ctx, &chat)
	if err != nil {
		s.httpError(w, err)
		return
	}
	s.render(w, "base", pd)
}

// handleCreateChat creates a new chat and redirects there via HTMX.
func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Remove orphaned empty chats before creating a new one.
	if _, err := s.store.DeleteEmptyChats(ctx, 0); err != nil {
		slog.Warn("clean up empty chats", "err", err)
	}
	id, err := s.store.CreateChat(ctx, untitled)
	if err != nil {
		s.httpError(w, err)
		return
	}
	redirect(w, r, "/chat/"+strconv.FormatInt(id, 10))
}

// handleDeleteChat removes a chat.
func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteChat(r.Context(), id); err != nil {
		s.httpError(w, err)
		return
	}
	redirect(w, r, "/")
}

// handleStats shows the persistent token usage statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	summary, err := s.store.UsageSummaryTotals(ctx)
	if err != nil {
		s.httpError(w, err)
		return
	}
	days, err := s.store.UsageByDay(ctx, 30)
	if err != nil {
		s.httpError(w, err)
		return
	}
	models, err := s.store.UsageByModel(ctx)
	if err != nil {
		s.httpError(w, err)
		return
	}
	// Requests answered by the router carry no model name.
	for i := range models {
		if models[i].Model == "" {
			models[i].Model = s.t("stats.auto_router")
		}
	}
	data := struct {
		Title   string
		Summary storage.UsageSummary
		Days    []storage.UsageDay
		Models  []storage.UsageModel
	}{
		Title:   s.t("stats.title"),
		Summary: summary,
		Days:    days,
		Models:  models,
	}
	s.render(w, "stats", data)
}

// handleSend stores the user message and returns the streaming shell.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Resolve the chat ID ("new" creates a chat on demand).
	idParam := chi.URLParam(r, "id")
	var chatID int64
	if idParam == "new" {
		newID, err := s.store.CreateChat(ctx, untitled)
		if err != nil {
			s.httpError(w, err)
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

	// Only honor web search when it was requested AND is configured.
	web := r.FormValue("web") == "1" && s.search.Enabled()
	// Image mode replaces the chat answer with a generated image.
	image := r.FormValue("mode") == "image" && s.cfg.ImagesConfigured()
	if image {
		web = false
	}

	if _, err := s.store.AddMessage(ctx, chatID, "user", message); err != nil {
		s.httpError(w, err)
		return
	}

	// Derive a provisional title from the first message.
	titleChanged := false
	if isUntitled(chat.Title) {
		newTitle := makeTitle(message)
		if err := s.store.UpdateChatTitle(ctx, chatID, newTitle); err == nil {
			chat.Title = newTitle
			titleChanged = true
		}
	} else {
		_ = s.store.TouchChat(ctx, chatID)
	}

	// Append the user bubble plus the streaming shell.
	s.render(w, "message", storage.Message{Role: "user", Content: message})
	s.render(w, "assistant-stream", struct {
		ChatID int64
		Web    bool
		Image  bool
	}{ChatID: chatID, Web: web, Image: image})
	if titleChanged {
		s.render(w, "title-oob", struct{ Title string }{Title: chat.Title})
	}
}

// handleGenerate streams the assistant answer as SSE.
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, s.t("stream.not_supported"), http.StatusInternalServerError)
		return
	}

	// The SSE stream is best effort: once the client disconnects, writes fail
	// and there is nothing useful left to report, so the errors are ignored.
	fail := func(msg string) {
		_ = sse.send("token", renderMarkdownString("⚠ "+msg))
		_ = sse.send("done", "")
	}

	if !s.cfg.IsConfigured() {
		fail(s.t("stream.not_configured"))
		return
	}

	history, err := s.store.ListMessages(ctx, id)
	if err != nil || len(history) == 0 {
		fail(s.t("stream.no_message"))
		return
	}

	// Use the last user message as the search query.
	query := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			query = history[i].Content
			break
		}
	}

	// Image mode: no streamed answer, the endpoint returns a finished image.
	if r.URL.Query().Get("image") == "1" {
		if !s.cfg.ImagesConfigured() {
			fail(s.t("stream.image_not_configured"))
			return
		}
		s.generateImage(ctx, sse, id, query, fail)
		return
	}

	// Web search: forced via the toggle (?web=1) or automatic via tool calling.
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
		// Fall back without tools if the router does not support tool calling.
		if streamErr != nil && acc.Len() == 0 {
			slog.Warn("tool-calling failed, falling back without tools", "err", streamErr)
			result, streamErr = s.llm.ChatStream(ctx, messages, onDelta)
		}
	} else {
		result, streamErr = s.llm.ChatStream(ctx, messages, onDelta)
	}

	if streamErr != nil {
		slog.Error("chat-stream", "err", streamErr)
		if acc.Len() == 0 {
			fail(s.t("stream.request_failed", streamErr.Error()))
			return
		}
		// A partial answer exists: mark it and carry on.
		acc.WriteString("\n\n" + s.t("stream.interrupted"))
		_ = sse.send("token", renderMarkdownString(acc.String()))
	}

	final := acc.String()
	if final != "" {
		// Deliberately not r.Context(): the answer must be persisted even when
		// the browser has already navigated away.
		if _, err := s.store.AddMessage(context.Background(), id, "assistant", final); err != nil {
			slog.Error("save assistant message", "err", err)
		}
	}

	// Show the model that was actually used (as reported by the router).
	if result.Model != "" {
		_ = sse.send("model", s.renderString("model-tag", result.Model))
	}

	// Emit the token usage of this answer as the message footer.
	if result.Usage.TotalTokens > 0 {
		_ = sse.send("usage", s.t("usage.footer",
			s.thousands(int64(result.Usage.TotalTokens)),
			s.thousands(int64(result.Usage.PromptTokens)),
			s.thousands(int64(result.Usage.CompletionTokens))))
	}

	// Generate a concise chat title after the first answer.
	if final != "" {
		s.maybeGenerateTitle(context.Background(), sse, id)
	}
	_ = sse.send("done", "")
}

// maybeGenerateTitle creates a short, meaningful chat title from the content
// after the first exchange and updates header and sidebar via SSE.
func (s *Server) maybeGenerateTitle(ctx context.Context, sse *sseWriter, chatID int64) {
	msgs, err := s.store.ListMessages(ctx, chatID)
	if err != nil || len(msgs) != 2 { // only on the first exchange (1 question + 1 answer)
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

	titleMessages := []llm.Message{
		{Role: "system", Content: s.t("prompt.title_system")},
		{Role: "user", Content: s.t("prompt.title_user",
			truncateRunes(userMsg, 800), truncateRunes(assistantMsg, 800))},
	}

	var sb strings.Builder
	if _, err := s.llm.ChatStream(ctx, titleMessages, func(delta string) error {
		sb.WriteString(delta)
		return nil
	}); err != nil {
		slog.Warn("generate title", "err", err)
		return
	}

	title := cleanTitle(sb.String())
	if title == "" {
		return
	}
	if err := s.store.UpdateChatTitle(ctx, chatID, title); err != nil {
		slog.Warn("save title", "err", err)
		return
	}

	// Update header and sidebar live.
	chats, _ := s.store.ListChats(ctx)
	chat, _ := s.store.GetChat(ctx, chatID)
	data := struct {
		Title       string
		Chats       []storage.Chat
		CurrentChat *storage.Chat
	}{Title: title, Chats: chats, CurrentChat: &chat}
	_ = sse.send("title", s.renderString("title-update", data))
}

// cleanTitle normalizes a title produced by the model.
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

// truncateRunes shortens a text to at most n runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// buildLLMMessages assembles the message list including RAG context and
// optional web context (RAG is limited to the current chat).
func (s *Server) buildLLMMessages(ctx context.Context, chatID int64, cfg config.Config, history []storage.Message, query string, web bool) []llm.Message {
	system := cfg.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = s.t("prompt.default_system")
	}

	// Fetch relevant document sections (only if embeddings are configured).
	if cfg.EmbeddingDeployment != "" && query != "" {
		results, err := s.retriever.Retrieve(ctx, chatID, query, retrievalTopK)
		if err != nil {
			slog.Warn("retrieval failed", "err", err)
		} else if len(results) > 0 {
			var sb strings.Builder
			sb.WriteString(s.t("prompt.rag_intro"))
			for i, res := range results {
				sb.WriteString(s.t("prompt.rag_item", i+1, res.Text))
			}
			system += sb.String()
		}
	}

	// Include current web results when requested.
	if web && query != "" {
		results, err := s.search.Search(ctx, query)
		if err != nil {
			slog.Warn("web search failed", "err", err)
		} else if len(results) > 0 {
			var sb strings.Builder
			sb.WriteString(s.t("prompt.web_intro"))
			for i, res := range results {
				sb.WriteString(s.t("prompt.web_item", i+1, res.Title, res.URL, res.Content))
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

// maxToolIterations bounds the tool loop to prevent endless round trips.
const maxToolIterations = 4

// webSearchTool defines the web search tool offered to the model.
func (s *Server) webSearchTool() llm.Tool {
	parameters, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": s.t("prompt.tool_query"),
			},
		},
		"required": []string{"query"},
	})
	if err != nil {
		// The schema is built from constants, so this cannot fail in practice.
		slog.Error("build tool schema", "err", err)
	}
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "web_search",
			Description: s.t("prompt.tool_desc"),
			Parameters:  parameters,
		},
	}
}

// streamWithSearch runs the tool loop: the model may call the web_search tool
// itself, results are fed back until a final answer (without a tool call) is
// streamed.
func (s *Server) streamWithSearch(ctx context.Context, sse *sseWriter, messages []llm.Message, onDelta func(string) error) (llm.ChatResult, error) {
	tools := []llm.Tool{s.webSearchTool()}
	var final llm.ChatResult

	for range maxToolIterations {
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

		// No tool calls -> the final answer has already been streamed.
		if len(turn.ToolCalls) == 0 {
			return final, nil
		}

		// Append the assistant message with the requested tool calls.
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   turn.Content,
			ToolCalls: turn.ToolCalls,
		})
		// Execute every tool call and feed the result back.
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

	// Iteration limit reached: force a last answer without tools.
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

// executeToolCall runs a tool call and returns the result as text for the
// model. Currently only "web_search" is supported.
func (s *Server) executeToolCall(ctx context.Context, sse *sseWriter, tc llm.ToolCall) string {
	if tc.Function.Name != "web_search" {
		return s.t("tool.unknown", tc.Function.Name)
	}

	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		slog.Warn("parse tool arguments", "err", err)
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return s.t("tool.empty_query")
	}

	// Show the status in the UI (best effort, see handleGenerate).
	_ = sse.send("tool", s.renderString("tool-status", query))

	results, err := s.search.Search(ctx, query)
	if err != nil {
		slog.Warn("web search (tool) failed", "query", query, "err", err)
		return s.t("tool.search_failed", err.Error())
	}
	if len(results) == 0 {
		return s.t("tool.no_results")
	}

	var sb strings.Builder
	sb.WriteString(s.t("tool.results_intro"))
	for i, res := range results {
		sb.WriteString(s.t("tool.result_item", i+1, res.Title, res.URL, res.Content))
	}
	return sb.String()
}

// handleConfigGet returns the settings dialog.
func (s *Server) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	s.renderConfig(w, false)
}

// handleConfigPost stores the configuration.
func (s *Server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	cfg := s.cfg.Get()
	previousLanguage := cfg.Language
	locks := s.cfg.Locks()
	// Endpoint fields locked via environment variables are disabled in the form
	// (and therefore not submitted); they must not be cleared. Save() protects
	// them as well, this only avoids blanking them here.
	if !locks.Endpoint {
		cfg.Endpoint = strings.TrimSpace(r.FormValue("endpoint"))
	}
	if !locks.ChatDeployment {
		cfg.ChatDeployment = strings.TrimSpace(r.FormValue("chat_deployment"))
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
	if !locks.ImageEndpoint {
		cfg.ImageEndpoint = strings.TrimSpace(r.FormValue("image_endpoint"))
	}
	if !locks.ImageDeployment {
		cfg.ImageDeployment = strings.TrimSpace(r.FormValue("image_deployment"))
	}
	if !locks.ImageAPIVersion {
		cfg.ImageAPIVersion = strings.TrimSpace(r.FormValue("image_api_version"))
	}
	cfg.Language = i18n.Normalize(r.FormValue("language"))
	cfg.SearchProvider = strings.ToLower(strings.TrimSpace(r.FormValue("search_provider")))

	// The SearXNG base URL is fetched by the server, so it is validated before
	// it is stored (see websearch.ValidateEndpoint).
	searchEndpoint := strings.TrimSpace(r.FormValue("search_endpoint"))
	if err := websearch.ValidateEndpoint(searchEndpoint); err != nil {
		s.renderConfigNotice(w, s.t("error.invalid_endpoint", err.Error()), true)
		return
	}
	cfg.SearchEndpoint = searchEndpoint

	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("search_max_results"))); err == nil && n > 0 {
		cfg.SearchMaxResults = n
	}
	cfg.SearchAuto = r.FormValue("search_auto") == "on"
	cfg.SystemPrompt = r.FormValue("system_prompt")
	if t, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("temperature")), 64); err == nil {
		cfg.Temperature = t
	}

	if err := s.cfg.Save(cfg); err != nil {
		s.httpError(w, err)
		return
	}
	// Configuration changed: verification has to run again.
	s.ready.invalidate()

	// A language change affects the whole page, not just the dialog, so ask
	// htmx for a full reload instead of swapping a single fragment.
	if cfg.Language != previousLanguage && r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderConfig(w, true)
}

// handleVerify runs all readiness checks and returns the result.
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

// handleStatus returns the connection badge for the sidebar. The UI polls it
// periodically so connection failures become visible.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "status-badge", s.statusData())
}

// statusData prepares the data for the status badge.
func (s *Server) statusData() statusBadge {
	snap := s.ready.snapshot()
	docs, err := s.store.CountDocuments(context.Background())
	if err != nil {
		slog.Warn("count documents", "err", err)
	}
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
		b.Label = s.t("status.not_configured")
		b.Level = "warn"
	case !snap.Checked:
		b.Label = s.t("status.checking")
		b.Level = "warn"
	case snap.AllOK:
		b.Label = s.t("status.connected")
		b.Level = "ok"
	case !snap.StorageOK:
		b.Label = s.t("status.storage_error")
		b.Level = "err"
	case !snap.ChatOK && !snap.EmbeddingOK:
		b.Label = s.t("status.endpoints_offline")
		b.Level = "err"
	case !snap.ChatOK:
		b.Label = s.t("status.chat_offline")
		b.Level = "err"
	default:
		b.Label = s.t("status.embedding_offline")
		b.Level = "err"
	}
	return b
}

// statusBadge holds the display data of the connection status.
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

// handleSetModel applies the model selection from the header menu. The choice
// is global and therefore survives switching between chats.
func (s *Server) handleSetModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if err := s.cfg.SetChatModel(model); err != nil {
		slog.Warn("model selection rejected", "model", model, "err", err)
		http.Error(w, s.t("error.invalid_model"), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpload accepts a document and processes it (RAG ingestion).
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

	// Uploads are only allowed once storage and the embedding endpoint have
	// been verified, so no document enters the pipeline before the required
	// components are demonstrably ready.
	if !s.ready.uploadsAllowed() {
		s.renderDocList(w, r, chatID, s.t("upload.blocked"), true)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTotalUploadBytes)
	// #nosec G120 -- the request body is bounded by MaxBytesReader above and
	// multipartMemoryBytes caps how much of it is kept in memory.
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil {
		s.httpError(w, err)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll() // delete spilled temporary files
		}
	}()

	ecfg := s.cfg.Get()
	if ecfg.EmbeddingDeployment == "" || ecfg.EmbeddingHost() == "" || !s.cfg.HasEmbeddingAPIKey() {
		s.renderDocList(w, r, chatID, s.t("upload.embedding_missing"), true)
		return
	}

	var headers []*multipart.FileHeader
	if r.MultipartForm != nil {
		headers = r.MultipartForm.File["file"]
	}
	if len(headers) == 0 {
		s.renderDocList(w, r, chatID, s.t("upload.no_file"), true)
		return
	}

	var (
		added    int
		failures []string
	)
	for _, header := range headers {
		if header.Size > maxUploadBytes {
			failures = append(failures, s.t("upload.too_large", header.Filename))
			continue
		}
		data, err := readMultipartFile(header)
		if err != nil {
			slog.Error("read upload", "file", header.Filename, "err", err)
			failures = append(failures, s.t("upload.read_error", header.Filename))
			continue
		}
		mime := header.Header.Get("Content-Type")
		if _, _, err := s.ingestor.Ingest(ctx, chatID, header.Filename, mime, data); err != nil {
			slog.Error("ingest", "file", header.Filename, "err", err)
			failures = append(failures, s.t("upload.item_failed", header.Filename, err.Error()))
			continue
		}
		added++
	}

	notice, isErr := s.uploadSummary(added, failures)
	s.renderDocList(w, r, chatID, notice, isErr)
}

// readMultipartFile reads the full content of an uploaded file. The size has
// already been bounded by MaxBytesReader and the per-file check in handleUpload.
func readMultipartFile(header *multipart.FileHeader) ([]byte, error) {
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// The buffer is deliberately not pre-sized from header.Size: that value is
	// supplied by the client, so a forged size would trigger a large allocation
	// before a single byte is read. Growing the buffer while copying is bounded
	// by maxUploadBytes and costs little for the file sizes involved here.
	return io.ReadAll(io.LimitReader(f, maxUploadBytes))
}

// uploadSummary builds the status message for a (multi-file) upload.
func (s *Server) uploadSummary(added int, failures []string) (string, bool) {
	switch {
	case added > 0 && len(failures) == 0:
		if added == 1 {
			return s.t("upload.added_one"), false
		}
		return s.t("upload.added_many", added), false
	case added > 0 && len(failures) > 0:
		return s.t("upload.partial", added, strings.Join(failures, ", ")), true
	default:
		return s.t("upload.failed", strings.Join(failures, ", ")), true
	}
}

// handleDeleteDocument removes a document from a chat.
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
		s.httpError(w, err)
		return
	}
	s.renderDocList(w, r, chatID, "", false)
}

// ---- render helpers ----

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template", "name", name, "err", err)
	}
}

// renderString renders a template into a string (for SSE events).
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

// renderConfigNotice renders the settings dialog with a status message.
func (s *Server) renderConfigNotice(w http.ResponseWriter, notice string, isErr bool) {
	s.renderConfigData(w, false, notice, isErr)
}

func (s *Server) renderConfigData(w http.ResponseWriter, saved bool, notice string, noticeErr bool) {
	data := struct {
		Config             config.Config
		Locks              config.Locks
		Languages          []i18n.Option
		HasKey             bool
		HasEmbeddingKey    bool
		HasOwnEmbeddingKey bool
		HasImageKey        bool
		HasOwnImageKey     bool
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
		Languages:          i18n.Options(),
		HasKey:             s.cfg.HasAPIKey(),
		HasEmbeddingKey:    s.cfg.HasEmbeddingAPIKey(),
		HasOwnEmbeddingKey: s.cfg.HasOwnEmbeddingAPIKey(),
		HasImageKey:        s.cfg.HasImageAPIKey(),
		HasOwnImageKey:     s.cfg.HasOwnImageAPIKey(),
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
		s.httpError(w, err)
		return
	}
	s.render(w, "doclist", pageData{ChatID: chatID, Documents: docs, Notice: notice, NoticeErr: isErr})
}

// renderMarkdownString renders Markdown into an HTML string (for SSE).
func renderMarkdownString(src string) string {
	return string(renderMarkdown(src))
}

// ---- misc helpers ----

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// redirect sets the HX-Redirect header for HTMX requests, otherwise it performs
// a classic HTTP redirect.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// httpError logs the cause and returns a generic message, so internal details
// never reach the client.
func (s *Server) httpError(w http.ResponseWriter, err error) {
	slog.Error("handler", "err", err)
	http.Error(w, s.t("error.internal"), http.StatusInternalServerError)
}

// makeTitle derives a short chat title from the first message.
func makeTitle(msg string) string {
	msg = strings.TrimSpace(strings.ReplaceAll(msg, "\n", " "))
	runes := []rune(msg)
	if len(runes) > 40 {
		return strings.TrimSpace(string(runes[:40])) + "…"
	}
	return msg
}

// humanBytes formats a byte size as a readable string (e.g. "2.4 MB").
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
