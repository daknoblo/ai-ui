package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/docparse"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/logbuf"
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
	// maxContextImages bounds how many attached images travel with a chat
	// request. Each of them is inlined as base64 and billed as prompt tokens.
	maxContextImages = 8
	// maxContextImageBytes bounds their total size before base64 encoding,
	// which inflates the payload by roughly a third.
	maxContextImageBytes = 16 << 20
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
	Title             string
	Chats             []storage.Chat
	CurrentChat       *storage.Chat
	Messages          []storage.Message
	Streams           map[int64]*streamView
	Documents         []storage.Document
	SourceImages      []storage.Image
	Configured        bool
	ChatID            int64
	Notice            string
	NoticeErr         bool
	ChatMode          string
	UploadsReady      bool
	UploadAccept      string
	SearchEnabled     bool
	ImageEnabled      bool
	ImageEditsEnabled bool
	ImageSize         string
	ImageQuality      string
	ImageFormat       string
	ImageSizes        []string
	ImageQualities    []string
	ImageFormats      []string
	Reasoning         reasoningView
	StatusBadge       statusBadge
}

// reasoningView is the model of the reasoning effort picker in the composer.
// The offered values depend on the model of the chat, so the picker is also
// re-rendered out of band whenever the model changes.
type reasoningView struct {
	ChatID  int64
	Efforts []string
	Current string
	OOB     bool
}

// imageModelOf returns the image deployment a chat generates with.
func imageModelOf(cfg config.Config, chat *storage.Chat) string {
	if chat != nil && chat.ImageModel != "" {
		return chat.ImageModel
	}
	return cfg.ImageDeployment
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
		Title:             "AI UI",
		Chats:             chats,
		CurrentChat:       current,
		Configured:        s.cfg.IsConfigured(),
		UploadsReady:      s.ready.uploadsAllowed(),
		UploadAccept:      docparse.UploadAccept(),
		SearchEnabled:     s.search.Enabled(),
		ImageEnabled:      s.cfg.ImagesConfigured(),
		ImageEditsEnabled: !cfg.Foundry || slices.Contains(cfg.ImageEditModels, imageModelOf(cfg, current)),
		ImageSize:         cfg.ImageSize,
		ImageQuality:      cfg.ImageQuality,
		ImageFormat:       cfg.ImageFormat,
		ImageSizes:        imageSizes,
		ImageQualities:    imageQualities,
		ImageFormats:      imageFormats,
		StatusBadge:       s.statusData(),
	}
	model := s.defaultChatModel()
	pd.Reasoning = reasoningView{
		Efforts: llm.ReasoningEfforts(s.cfg.ModelIdentity(model)),
		Current: llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(model), cfg.ReasoningEffort),
	}
	if current != nil {
		msgs, turns, err := s.store.Conversation(ctx, current.ID)
		if err != nil {
			return pageData{}, err
		}
		docs, err := s.store.ListDocumentsByChat(ctx, current.ID)
		if err != nil {
			return pageData{}, err
		}
		imgs, err := s.store.ListImagesByKind(ctx, current.ID, storage.ImageUpload)
		if err != nil {
			return pageData{}, err
		}
		pd.Messages = msgs
		pd.Streams = make(map[int64]*streamView)
		for _, turn := range turns {
			if turn.Active() {
				view, err := generationView(turn)
				if err != nil {
					return pageData{}, err
				}
				pd.Streams[turn.ResponseID] = &view
			} else if turn.State == storage.GenerationInterrupted {
				for i := range pd.Messages {
					if pd.Messages[i].ID == turn.ResponseID {
						pd.Messages[i].Content += "\n\n⚠ " + s.t("stream.recovered_interrupt")
					}
				}
			}
		}
		pd.Documents = docs
		pd.SourceImages = imgs
		pd.Title = s.chatTitle(current.Title)
		pd.ChatID = current.ID
		pd.ChatMode = current.Mode
		pd.Reasoning = reasoningView{
			ChatID:  current.ID,
			Efforts: llm.ReasoningEfforts(s.cfg.ModelIdentity(current.Model)),
			Current: llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(current.Model), current.ReasoningEffort),
		}
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

// defaultChatModel is the model a new chat starts with: the one last chosen,
// otherwise the first entry of AZURE_MODELS. Only an empty list leaves the
// choice to the router.
func (s *Server) defaultChatModel() string {
	cfg := s.cfg.Get()
	if cfg.ChatModel != "" {
		return cfg.ChatModel
	}
	if cfg.Foundry {
		return cfg.ChatDeployment
	}
	if len(cfg.ChatModels) > 0 {
		return cfg.ChatModels[0]
	}
	return ""
}

// newChat creates a chat with the defaults for model and reasoning effort.
func (s *Server) newChat(ctx context.Context) (int64, error) {
	model := s.defaultChatModel()
	return s.store.CreateChat(ctx, untitled, model,
		llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(model), s.cfg.Get().ReasoningEffort))
}

// handleIndex always opens a fresh chat and cleans up orphaned empty ones.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Remove orphaned empty chats before creating a new one.
	if _, err := s.store.DeleteEmptyChats(ctx, 0); err != nil {
		slog.Warn("clean up empty chats", "err", err)
	}
	id, err := s.newChat(ctx)
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
	id, err := s.newChat(ctx)
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
	if err := s.store.WithCorpusMutation(r.Context(), func() error {
		return s.store.DeleteChat(r.Context(), id)
	}); err != nil {
		s.corpusError(w, err)
		return
	}
	s.configMu.Lock()
	for _, job := range s.generations {
		if job.chatID == id {
			job.cancel()
		}
	}
	s.configMu.Unlock()
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
	docs, err := s.store.CountDocuments(ctx)
	if err != nil {
		s.httpError(w, err)
		return
	}
	diskBytes := s.store.DiskUsage()
	data := struct {
		Title     string
		Summary   storage.UsageSummary
		Days      []storage.UsageDay
		Models    []storage.UsageModel
		Chart     []chartBar
		DiskHuman string
		DocCount  int64
	}{
		Title:     s.t("stats.title"),
		Summary:   summary,
		Days:      days,
		Models:    models,
		Chart:     buildChart(days, 14),
		DiskHuman: humanBytes(diskBytes),
		DocCount:  int64(docs),
	}
	s.render(w, "stats", data)
}

// chartBar is one column of the usage chart.
type chartBar struct {
	Label   string
	Tokens  int64
	Percent int
}

// buildChart turns the most recent days into chronologically ordered bars whose
// height is relative to the busiest day.
func buildChart(days []storage.UsageDay, limit int) []chartBar {
	if len(days) > limit {
		days = days[:limit]
	}
	var max int64
	for _, d := range days {
		if d.TotalTokens > max {
			max = d.TotalTokens
		}
	}
	bars := make([]chartBar, 0, len(days))
	// UsageByDay returns the newest first; the chart reads left to right.
	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		percent := 0
		if max > 0 {
			percent = int(d.TotalTokens * 100 / max)
		}
		label := d.Day
		if len(label) == 10 { // YYYY-MM-DD -> DD.MM.
			label = label[8:10] + "." + label[5:7] + "."
		}
		bars = append(bars, chartBar{Label: label, Tokens: d.TotalTokens, Percent: percent})
	}
	return bars
}

// handleSend stores the user message and returns the streaming shell.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, s.t("stream.limit"), http.StatusRequestEntityTooLarge)
		return
	}

	// Resolve the chat ID ("new" creates a chat on demand).
	idParam := chi.URLParam(r, "id")
	var chatID int64
	if idParam == "new" {
		newID, err := s.newChat(ctx)
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
	image := r.FormValue("mode") == "image"
	if image {
		web = false
	}
	// In image mode the latest image of the chat is edited unless switched off.
	edit := image && r.FormValue("edit") == "1"

	cfg := s.cfg.Get()
	turn, err := s.submitGeneration(ctx, chat, message, generationOptions{
		Web: web, Image: image, Edit: edit,
		Chat: llm.ChatOptions{Model: chat.Model,
			ReasoningEffort: llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(chat.Model), chat.ReasoningEffort)},
		ImageOptions: llm.ImageOptions{Deployment: imageModelOf(cfg, &chat),
			Size: cfg.ImageSize, Quality: cfg.ImageQuality, Format: cfg.ImageFormat},
	})
	if errors.Is(err, storage.ErrGenerationBusy) {
		http.Error(w, s.t("stream.busy"), http.StatusConflict)
		return
	}
	if errors.Is(err, errGenerationCapacity) {
		slog.Info("generation submission rejected", "chat", chatID, "reason", err)
		w.Header().Set("Retry-After", "2")
		http.Error(w, s.t("stream.overloaded"), http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}

	// Derive a provisional title from the first message.
	titleChanged := false
	if isUntitled(chat.Title) {
		chat.Title = makeTitle(message)
		titleChanged = true
	}

	// Append the user bubble plus the streaming shell.
	s.render(w, "message", storage.Message{Role: "user", Content: message})
	s.render(w, "assistant-stream", streamView{ChatID: chatID, TurnID: turn.ID, Web: web, Image: image, Edit: edit})
	if titleChanged {
		s.render(w, "title-oob", struct{ Title string }{Title: chat.Title})
	}
}

// generateTurn runs once after its durable claim, independent of SSE observers.
func (s *Server) generateTurn(ctx context.Context, sse *generationStream, turn storage.Generation, options generationOptions) {
	id := turn.ChatID
	fail := func(msg string) {
		sse.state, sse.failure = storage.GenerationFailed, msg
		sse.content = "⚠ " + msg
		if ctx.Err() != nil {
			sse.state = storage.GenerationInterrupted
		}
	}

	if !s.cfg.IsConfigured() {
		fail(s.t("stream.not_configured"))
		return
	}

	history, err := s.store.GenerationHistory(ctx, id, turn.ID)
	if err != nil || len(history) == 0 || history[len(history)-1].ID != turn.ID || history[len(history)-1].Role != "user" {
		fail(s.t("stream.no_message"))
		return
	}
	query := history[len(history)-1].Content

	// Image mode: no streamed answer, the endpoint returns a finished image.
	if options.Image {
		if !s.cfg.ImagesConfigured() {
			fail(s.t("stream.image_not_configured"))
			return
		}
		s.generateImage(ctx, sse, id, query, options.Edit, llm.Usage{}, fail)
		return
	}

	// Web search: forced by the submitted toggle or automatic via tool calling.
	cfg := s.cfg.Get()
	forceWeb := options.Web && s.search.Enabled()
	autoWeb := cfg.SearchAuto && s.search.Enabled() && !forceWeb
	autoImages := s.cfg.ImagesConfigured()

	// The chat keeps its own model and reasoning effort; empty values leave both
	// to the router and the model.
	opts := options.Chat

	messages, err := s.buildLLMMessages(ctx, id, cfg, history, query, forceWeb)
	if err != nil {
		slog.Warn("document retrieval failed", "chat", id, "err", err)
		fail(s.t("stream.retrieval_failed", err.Error()))
		return
	}
	if autoImages {
		count, err := s.store.CountImages(ctx, id)
		if err != nil {
			slog.Warn("read image context", "err", err)
			fail(s.t("stream.request_failed", err.Error()))
			return
		}
		instructions := s.t("prompt.image_tools") + "\n" + s.t("prompt.image_context", count > 0)
		if len(messages) > 0 && messages[0].Role == "system" {
			messages[0].Content += "\n\n" + instructions
		} else {
			messages = append([]llm.Message{{Role: "system", Content: instructions}}, messages...)
		}
	}

	// A picture is useless to a model that cannot see. Rather than failing the
	// request, the answer is routed to a model that can - for this turn only, so
	// the picker the user set stays where it is. The model tag of the answer
	// names whatever actually replied.
	if messagesCarryImages(messages) {
		picked, ok := s.llm.VisionDeployment(opts.Model)
		switch {
		case !ok:
			if cfg.Foundry {
				fail(s.t("stream.vision_missing"))
				return
			}
			// Nothing configured can read images. Sending them anyway would be
			// rejected with a 400 - and because the attachments are re-read on
			// every turn, the chat would stay broken until the image is
			// deleted. Dropping them keeps the conversation usable and says so.
			slog.Warn("no vision capable deployment, answering without the attached images",
				"chat", id, "model", opts.Model)
			dropImages(messages)
			if err := sse.send("tool", s.renderString("turn-note", s.t("stream.images_dropped"))); err != nil {
				fail(s.t("stream.request_failed", err.Error()))
				return
			}
		case picked != opts.Model:
			slog.Info("switching to a vision capable model for the attachments",
				"from", opts.Model, "to", picked)
			opts.Model = picked
			opts.ReasoningEffort = llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(picked), cfg.ReasoningEffort)
		}
	}

	var acc strings.Builder
	onDelta := func(delta string) error {
		if len(delta) > maxResponseBytes-acc.Len() {
			return errors.New(s.t("stream.limit"))
		}
		acc.WriteString(delta)
		sse.content = acc.String()
		return sse.send("token", renderMarkdownString(acc.String()))
	}

	var (
		result    llm.ChatResult
		streamErr error
	)
	if autoWeb || autoImages {
		var imageHandled bool
		result, imageHandled, streamErr = s.streamWithTools(ctx, sse, id, opts, messages, autoWeb, autoImages, onDelta, fail)
		if imageHandled {
			return
		}
	} else {
		result, streamErr = s.llm.ChatStream(ctx, opts, messages, onDelta)
	}

	if streamErr != nil {
		slog.Error("chat-stream", "err", streamErr)
		if acc.Len() == 0 {
			fail(s.t("stream.request_failed", streamErr.Error()))
			return
		}
		// A partial answer exists: mark it and carry on.
		sse.state, sse.failure = storage.GenerationFailed, streamErr.Error()
		if ctx.Err() != nil {
			sse.state = storage.GenerationInterrupted
		}
		acc.WriteString("\n\n" + s.t("stream.interrupted"))
		_ = sse.send("token", renderMarkdownString(acc.String()))
	}

	final := acc.String()
	sse.content = final
	if final == "" && streamErr == nil {
		fail(s.t("stream.no_message"))
		return
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
	if final != "" && streamErr == nil {
		titleCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		s.maybeGenerateTitle(titleCtx, sse, id)
	}
	_ = sse.send("done", "")
}

// maybeGenerateTitle creates a short, meaningful chat title from the content
// after the first exchange and updates header and sidebar via SSE.
func (s *Server) maybeGenerateTitle(ctx context.Context, sse *generationStream, chatID int64) {
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
			assistantMsg = sse.content
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
	if _, err := s.llm.ChatStream(ctx, llm.ChatOptions{}, titleMessages, func(delta string) error {
		if len(delta) > 4096-sb.Len() {
			return errors.New("generated title exceeds size limit")
		}
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

	// SSE observers read the current title after the outcome is committed.
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

// buildLLMMessages assembles the message list including the attachments of the
// chat, the RAG context and optional web context (both limited to this chat).
func (s *Server) buildLLMMessages(ctx context.Context, chatID int64, cfg config.Config, history []storage.Message, query string, web bool) ([]llm.Message, error) {
	system := cfg.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = s.t("prompt.default_system")
	}

	// Files bound to the chat. Naming them in the prompt keeps a question like
	// "what is in the attachment?" answerable even when the vector search finds
	// nothing that matches the wording of the question.
	att := s.attachmentsOf(ctx, chatID)
	if len(att.names) > 0 {
		var sb strings.Builder
		sb.WriteString(s.t("prompt.files_intro"))
		for _, name := range att.names {
			sb.WriteString(s.t("prompt.files_item", name))
		}
		system += sb.String()
	}

	// Fetch relevant document sections (only if embeddings are configured).
	if cfg.EmbeddingDeployment != "" && query != "" {
		results, err := s.retriever.Retrieve(ctx, chatID, query, retrievalTopK)
		if err != nil {
			return nil, err
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
	attachImages(msgs, att.images)
	return msgs, nil
}

// chatAttachments is what the files of a chat contribute to a request: the
// image payloads travel inline with the message, the names describe every
// attachment the model has access to.
type chatAttachments struct {
	images []llm.ImageContent
	names  []string
}

// attachmentsOf collects the files bound to a chat. Documents contribute their
// name only - their text reaches the model through retrieval. Images are capped
// in count and total size, because each of them is inlined as base64.
func (s *Server) attachmentsOf(ctx context.Context, chatID int64) chatAttachments {
	var att chatAttachments

	docs, err := s.store.ListDocumentsByChat(ctx, chatID)
	if err != nil {
		slog.Warn("list attached documents", "chat", chatID, "err", err)
	}
	for _, doc := range docs {
		att.names = append(att.names, doc.Name)
	}

	images, err := s.store.ImagesWithDataByKind(ctx, chatID, storage.ImageUpload, maxContextImages)
	if err != nil {
		slog.Warn("list attached images", "chat", chatID, "err", err)
		return att
	}
	total := 0
	for _, img := range images {
		// An image that does not fit the budget is left out of the name list as
		// well, so the model is never told about something it cannot see.
		if total+len(img.Data) > maxContextImageBytes {
			slog.Warn("attached image exceeds the context budget", "chat", chatID, "image", img.Name)
			continue
		}
		total += len(img.Data)
		att.names = append(att.names, img.Name)
		att.images = append(att.images, llm.ImageContent{MIME: img.MIME, Data: img.Data})
	}
	return att
}

// attachImages hands the images to the most recent user message, which is the
// one the model is answering.
func attachImages(msgs []llm.Message, images []llm.ImageContent) {
	if len(images) == 0 {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			msgs[i].Images = images
			return
		}
	}
}

// messagesCarryImages reports whether a request needs a model that can see.
func messagesCarryImages(msgs []llm.Message) bool {
	return slices.ContainsFunc(msgs, func(m llm.Message) bool {
		return len(m.Images) > 0
	})
}

// dropImages strips the attachments from a request that no configured model
// could accept. The names stay in the system prompt, so the model still knows
// something was attached and can say that it cannot look at it.
func dropImages(msgs []llm.Message) {
	for i := range msgs {
		msgs[i].Images = nil
	}
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

// streamWithTools keeps ordinary answers in chat and delegates explicit image
// requests to the configured image model without changing the conversation mode.
func (s *Server) streamWithTools(ctx context.Context, sse *generationStream, chatID int64, opts llm.ChatOptions,
	messages []llm.Message, useWeb, useImages bool, onDelta func(string) error, fail func(string),
) (llm.ChatResult, bool, error) {
	var tools []llm.Tool
	if useWeb {
		tools = append(tools, s.webSearchTool())
	}
	if useImages {
		tools = append(tools, s.imageTool())
	}
	var final llm.ChatResult

	for range maxToolIterations {
		turn, err := s.llm.ChatStreamWithTools(ctx, opts, messages, tools, onDelta)
		if err != nil {
			return final, false, err
		}
		if turn.Model != "" {
			final.Model = turn.Model
		}
		final.Usage.PromptTokens += turn.Usage.PromptTokens
		final.Usage.CompletionTokens += turn.Usage.CompletionTokens
		final.Usage.TotalTokens += turn.Usage.TotalTokens

		// No tool calls -> the final answer has already been streamed.
		if len(turn.ToolCalls) == 0 {
			return final, false, nil
		}

		for _, call := range turn.ToolCalls {
			if call.Function.Name != "generate_image" {
				continue
			}
			if !useImages || len(turn.ToolCalls) != 1 || turn.FinishReason != "tool_calls" {
				return final, false, fmt.Errorf("image generation must be the only enabled tool call in its turn")
			}
			if err := s.executeImageTool(ctx, sse, chatID, call, final.Usage, fail); err != nil {
				return final, false, err
			}
			return final, true, nil
		}
		// Append the assistant message with the requested tool calls.
		messages = append(messages, llm.Message{
			Role:          "assistant",
			Content:       turn.Content,
			ToolCalls:     turn.ToolCalls,
			ResponseItems: turn.ResponseItems,
		})
		// Execute every tool call and feed the result back.
		for _, tc := range turn.ToolCalls {
			if !useWeb || tc.Function.Name != "web_search" {
				return final, false, fmt.Errorf("model requested an unavailable tool: %s", tc.Function.Name)
			}
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
	turn, err := s.llm.ChatStream(ctx, opts, messages, onDelta)
	if err != nil {
		return final, false, err
	}
	if turn.Model != "" {
		final.Model = turn.Model
	}
	final.Usage.PromptTokens += turn.Usage.PromptTokens
	final.Usage.CompletionTokens += turn.Usage.CompletionTokens
	final.Usage.TotalTokens += turn.Usage.TotalTokens
	return final, false, nil
}

// executeToolCall runs a tool call and returns the result as text for the
// model. Currently only "web_search" is supported.
func (s *Server) executeToolCall(ctx context.Context, sse *generationStream, tc llm.ToolCall) string {
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

// submitted returns a trimmed form value and whether the field was part of the
// form at all. Fields the dialog hides must keep their stored value.
func submitted(r *http.Request, name string) (string, bool) {
	if _, ok := r.Form[name]; !ok {
		return "", false
	}
	return strings.TrimSpace(r.FormValue(name)), true
}

// reasoningEfforts are the selectable values of the reasoning effort. Which of
// them a model accepts differs; "auto" omits the parameter, and a rejected
// value is dropped by the client (see llm.chatRequest.dropRejected).
var reasoningEfforts = []string{"auto", "none", "minimal", "low", "medium", "high", "xhigh"}

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
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg := s.cfg.Get()
	previousLanguage := cfg.Language
	previous := cfg
	locks := s.cfg.Locks()
	// Endpoint fields locked via environment variables are disabled in the form
	// (and therefore not submitted); they must not be cleared. Save() protects
	// them as well, this only avoids blanking them here.
	if !cfg.Foundry && !locks.Endpoint {
		cfg.Endpoint = strings.TrimSpace(r.FormValue("endpoint"))
	}
	if !locks.ChatDeployment {
		cfg.ChatDeployment = strings.TrimSpace(r.FormValue("chat_deployment"))
	}
	if !cfg.Foundry && !locks.APIVersion {
		if v, ok := submitted(r, "api_version"); ok {
			cfg.APIVersion = v
		}
	}
	if !cfg.Foundry && !locks.EmbeddingEndpoint {
		cfg.EmbeddingEndpoint = strings.TrimSpace(r.FormValue("embedding_endpoint"))
	}
	if !locks.EmbeddingDeployment {
		cfg.EmbeddingDeployment = strings.TrimSpace(r.FormValue("embedding_deployment"))
	}
	if !cfg.Foundry && !locks.EmbeddingAPIVersion {
		if v, ok := submitted(r, "embedding_api_version"); ok {
			cfg.EmbeddingAPIVersion = v
		}
	}
	if !cfg.Foundry && !locks.ImageEndpoint {
		cfg.ImageEndpoint = strings.TrimSpace(r.FormValue("image_endpoint"))
	}
	if !locks.ImageDeployment {
		cfg.ImageDeployment = strings.TrimSpace(r.FormValue("image_deployment"))
	}
	if !cfg.Foundry && !locks.ImageAPIVersion {
		if v, ok := submitted(r, "image_api_version"); ok {
			cfg.ImageAPIVersion = v
		}
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
	cfg.LogLevel = logbuf.NormalizeLevel(r.FormValue("log_level"))
	if t, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("temperature")), 64); err == nil {
		cfg.Temperature = t
	}
	if effort := strings.ToLower(strings.TrimSpace(r.FormValue("reasoning_effort"))); slices.Contains(reasoningEfforts, effort) {
		cfg.ReasoningEffort = effort
	}
	if cfg.Foundry {
		cfg.VisionDeployment = strings.TrimSpace(r.FormValue("vision_deployment"))
		if err := s.cfg.ValidateRoleSelections(cfg); err != nil {
			s.renderConfigNotice(w, s.t("foundry.invalid_selection", err.Error()), true)
			return
		}
		cfg.ChatModel = cfg.ChatDeployment
	}
	if cfg.EmbeddingHost() != previous.EmbeddingHost() ||
		cfg.EmbeddingVersion() != previous.EmbeddingVersion() ||
		cfg.EmbeddingDeployment != previous.EmbeddingDeployment {
		job, exists, err := s.store.LatestReindex(r.Context())
		if err != nil {
			s.httpError(w, err)
			return
		}
		if exists && job.Status == "running" {
			s.renderConfigNotice(w, s.t("foundry.reindex_busy"), true)
			return
		}
	}

	if err := s.cfg.Save(cfg); err != nil {
		s.httpError(w, err)
		return
	}
	s.applyLogLevel(cfg.LogLevel)
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
	results := s.runChecks(r.Context(), true)
	_, checkedAt := s.ready.lastResults()
	data := struct {
		Results        []checkResult
		CheckedAt      string
		Verified       bool
		UploadsAllowed bool
		StatusBadge    statusBadge
	}{
		Results:        results,
		CheckedAt:      formatCheckTime(checkedAt),
		Verified:       s.ready.verified(),
		UploadsAllowed: s.ready.uploadsAllowed(),
		StatusBadge:    s.statusData(),
	}
	s.render(w, "verify-results", data)
}

// formatCheckTime renders when a check ran; an empty result means "never".
func formatCheckTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Local().Format("2006-01-02 15:04:05")
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
	Label      string
	Level      string // ok | warn | err
}

// handleSetModel retains the existing model-selection endpoint for API clients
// and pages opened before the header selector was removed.
func (s *Server) handleSetModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	chatID, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	chat, err := s.store.GetChat(r.Context(), chatID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cfg := s.cfg.Get()

	// Image selections are stored separately from the chat model.
	if chat.Mode == storage.ChatModeImage {
		if model != "" && !slices.Contains(cfg.ImageModels, model) {
			slog.Warn("image model selection rejected", "model", model)
			http.Error(w, s.t("error.invalid_model"), http.StatusBadRequest)
			return
		}
		if cfg.Foundry {
			if _, err := s.cfg.ResolveDeployment(foundry.Images, model); err != nil {
				slog.Warn("image deployment selection rejected", "err", err)
				http.Error(w, s.t("error.invalid_model"), http.StatusBadRequest)
				return
			}
		}
		if err := s.store.UpdateChatImageModel(r.Context(), chatID, model); err != nil {
			s.httpError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := s.cfg.SetChatModel(model); err != nil {
		slog.Warn("model selection rejected", "model", model, "err", err)
		http.Error(w, s.t("error.invalid_model"), http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateChatModel(r.Context(), chatID, model); err != nil {
		s.httpError(w, err)
		return
	}

	// The offered efforts belong to the model, so the picker is re-rendered and
	// a value the new model does not know falls back to "auto".
	effort := llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(model), chat.ReasoningEffort)
	if err := s.store.UpdateChatReasoningEffort(r.Context(), chatID, effort); err != nil {
		s.httpError(w, err)
		return
	}
	s.render(w, "reasoning-opt", reasoningView{
		ChatID:  chatID,
		Efforts: llm.ReasoningEfforts(s.cfg.ModelIdentity(model)),
		Current: effort,
		OOB:     true,
	})
}

// handleSetReasoning stores the reasoning effort of a chat. Which values are
// valid depends on the model the chat is pinned to.
func (s *Server) handleSetReasoning(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	chatID, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chat, err := s.store.GetChat(r.Context(), chatID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	effort := llm.NormalizeReasoningEffort(s.cfg.ModelIdentity(chat.Model), r.FormValue("reasoning_effort"))
	if err := s.store.UpdateChatReasoningEffort(r.Context(), chatID, effort); err != nil {
		s.httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetMode stores the answer mode of a chat so switching chats restores
// what that conversation was last used for.
func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, err)
		return
	}
	chatID, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mode := storage.ChatModeChat
	if r.FormValue("mode") == storage.ChatModeImage {
		mode = storage.ChatModeImage
	}
	if err := s.store.UpdateChatMode(r.Context(), chatID, mode); err != nil {
		s.httpError(w, err)
		return
	}

	if _, err := s.store.GetChat(r.Context(), chatID); err != nil {
		s.httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpload accepts documents (RAG ingestion) and, when image generation is
// configured, images that serve as the source for editing.
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

	var headers []*multipart.FileHeader
	if r.MultipartForm != nil {
		headers = r.MultipartForm.File["file"]
	}
	if len(headers) == 0 {
		s.renderDocList(w, r, chatID, s.t("upload.no_file"), true)
		return
	}

	// Images become an attachment the model can look at (and the source for
	// image editing); everything else is ingested as a document. That split does
	// not depend on the image endpoint: an attachment only needs a vision
	// capable chat model, which is configured elsewhere.
	var images, docs []*multipart.FileHeader
	for _, header := range headers {
		if uploadImageMIME(header) != "" {
			images = append(images, header)
			continue
		}
		docs = append(docs, header)
	}

	// Document ingestion needs the embedding endpoint; images do not.
	if len(docs) > 0 {
		// Uploads are only allowed once storage and the embedding endpoint have
		// been verified, so no document enters the pipeline before the required
		// components are demonstrably ready.
		if !s.ready.uploadsAllowed() {
			s.renderDocList(w, r, chatID, s.t("upload.blocked"), true)
			return
		}
		ecfg := s.cfg.Get()
		if ecfg.EmbeddingDeployment == "" || ecfg.EmbeddingHost() == "" || !s.cfg.HasEmbeddingCredentials() {
			s.renderDocList(w, r, chatID, s.t("upload.embedding_missing"), true)
			return
		}
	}

	var (
		added    int
		failures []string
		// transcribed names the scans that had to be read by the model.
		transcribed []string
	)
	for _, header := range images {
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
		if _, err := s.store.AddImage(ctx, chatID, storage.ImageUpload,
			header.Filename, "", uploadImageMIME(header), data); err != nil {
			slog.Error("save source image", "file", header.Filename, "err", err)
			failures = append(failures, s.t("upload.item_failed", header.Filename, err.Error()))
			continue
		}
		added++
	}
	for _, header := range docs {
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
		res, err := s.ingestor.Ingest(ctx, chatID, header.Filename, mime, data)
		if err != nil {
			slog.Error("ingest", "file", header.Filename, "err", err)
			failures = append(failures, s.t("upload.item_failed", header.Filename, err.Error()))
			continue
		}
		// A scan was read by the model rather than parsed. That is worth saying:
		// a transcription can be imperfect in a way a parsed document is not.
		if res.OCRPages > 0 {
			transcribed = append(transcribed, s.t("upload.ocr_item", header.Filename, res.OCRPages))
		}
		added++
	}

	notice, isErr := s.uploadSummary(added, failures)
	if len(transcribed) > 0 {
		notice += " " + s.t("upload.ocr_used", strings.Join(transcribed, ", "))
	}
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
	if err := s.store.WithCorpusMutation(r.Context(), func() error {
		return s.store.DeleteDocumentForChat(r.Context(), chatID, docID)
	}); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.corpusError(w, err)
		return
	}
	s.renderDocList(w, r, chatID, "", false)
}

func (s *Server) corpusError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrReindexInProgress) {
		slog.Info("document mutation deferred during reindex")
		http.Error(w, s.t("foundry.reindex_busy"), http.StatusConflict)
		return
	}
	s.httpError(w, err)
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
	cfg := s.cfg.Get()
	results, checkedAt := s.ready.lastResults()
	results = s.currentInventoryResults(results)
	data := struct {
		Config             config.Config
		Foundry            foundryView
		Locks              config.Locks
		Languages          []i18n.Option
		HasKey             bool
		HasEmbeddingKey    bool
		HasOwnEmbeddingKey bool
		HasImageKey        bool
		HasOwnImageKey     bool
		ImageKeyMismatch   bool
		HasSearchKey       bool
		SearchEnabled      bool
		LogLevels          []string
		ReasoningEfforts   []string
		// The api-version only exists on the classic schema; on the v1 surface
		// the client decides it, so the fields stay out of the dialog.
		ShowAPIVersion          bool
		ShowEmbeddingAPIVersion bool
		ShowImageAPIVersion     bool
		Results                 []checkResult
		CheckedAt               string
		Saved                   bool
		Verified                bool
		UploadsAllowed          bool
		Notice                  string
		NoticeErr               bool
	}{
		Config:                  cfg,
		Foundry:                 s.foundryData(s.ctx),
		Locks:                   s.cfg.Locks(),
		Languages:               i18n.Options(),
		ShowAPIVersion:          !llm.IsV1Endpoint(cfg.Endpoint),
		ShowEmbeddingAPIVersion: !llm.IsV1Endpoint(cfg.EmbeddingHost()),
		ShowImageAPIVersion:     !llm.IsV1Endpoint(cfg.ImageHost()),
		Results:                 results,
		CheckedAt:               formatCheckTime(checkedAt),
		HasKey:                  s.cfg.HasAPIKey(),
		HasEmbeddingKey:         s.cfg.HasEmbeddingAPIKey(),
		HasOwnEmbeddingKey:      s.cfg.HasOwnEmbeddingAPIKey(),
		HasImageKey:             s.cfg.HasImageAPIKey(),
		HasOwnImageKey:          s.cfg.HasOwnImageAPIKey(),
		ImageKeyMismatch:        crossResourceImageKey(cfg, s.cfg.HasOwnImageAPIKey()),
		HasSearchKey:            s.cfg.HasSearchAPIKey(),
		SearchEnabled:           s.search.Enabled(),
		LogLevels:               logbuf.Levels,
		ReasoningEfforts:        reasoningEfforts,
		Saved:                   saved,
		Verified:                s.ready.verified(),
		UploadsAllowed:          s.ready.uploadsAllowed(),
		Notice:                  notice,
		NoticeErr:               noticeErr,
	}
	s.render(w, "config", data)
}

func (s *Server) renderDocList(w http.ResponseWriter, r *http.Request, chatID int64, notice string, isErr bool) {
	docs, err := s.store.ListDocumentsByChat(r.Context(), chatID)
	if err != nil {
		s.httpError(w, err)
		return
	}
	imgs, err := s.store.ListImagesByKind(r.Context(), chatID, storage.ImageUpload)
	if err != nil {
		s.httpError(w, err)
		return
	}
	s.render(w, "doclist", pageData{ChatID: chatID, Documents: docs, SourceImages: imgs, Notice: notice, NoticeErr: isErr})
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
