package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/daknoblo/ai-ui/internal/llm"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const (
	generationTimeout        = 10 * time.Minute
	maxConcurrentGenerations = 4
	maxResponseBytes         = 1 << 20
	maxSnapshotBytes         = 8 << 20
)

var errGenerationCapacity = errors.New("generation worker capacity reached")

type generationOptions struct {
	Web, Image, Edit bool
	Chat             llm.ChatOptions
	ImageOptions     llm.ImageOptions
	SourceImageID    int64
}

type generationJob struct {
	chatID int64
	cancel context.CancelFunc
}

type streamView struct {
	ChatID, TurnID   int64
	Web, Image, Edit bool
}

func generationView(g storage.Generation) (streamView, error) {
	var opts generationOptions
	err := json.Unmarshal([]byte(g.Request), &opts)
	return streamView{ChatID: g.ChatID, TurnID: g.ID, Web: opts.Web, Image: opts.Image, Edit: opts.Edit}, err
}

// generationStream is owned by one worker. Only a bounded current snapshot is
// retained, and readers replay it from SQLite independently of browser lifetime.
type generationStream struct {
	server        *Server
	ctx           context.Context
	turn          storage.Generation
	options       generationOptions
	events        map[string]string
	content       string
	state         string
	failure       string
	image         *storage.Image
	lastSave      time.Time
	snapshotError error
}

func (g *generationStream) send(event, data string) (err error) {
	defer func() {
		if err != nil && g.snapshotError == nil {
			g.snapshotError = err
		}
	}()
	if event == "done" || event == "title" {
		return nil // Only the committed terminal outcome may close a browser stream.
	}
	if len(data) > maxSnapshotBytes {
		return errors.New("generation snapshot exceeds size limit")
	}
	if event == "tool" {
		// Tool iterations are bounded; replay replaces this cumulative view.
		data = g.events[event] + data
		if len(data) > maxResponseBytes {
			return errors.New("generation tool output exceeds size limit")
		}
	}
	size := len(data)
	for name, value := range g.events {
		if name != event {
			size += len(value)
		}
	}
	if size > maxSnapshotBytes {
		return errors.New("generation snapshot exceeds size limit")
	}
	previous := g.events[event]
	g.events[event] = data
	if event == "token" && time.Since(g.lastSave) < 100*time.Millisecond {
		return g.ctx.Err()
	}
	snapshot, err := json.Marshal(g.events)
	if err != nil {
		return err
	}
	if len(snapshot) > maxSnapshotBytes {
		g.events[event] = previous
		return errors.New("generation snapshot exceeds size limit")
	}
	if err := g.server.store.UpdateGeneration(g.ctx, g.turn.ID, g.content, string(snapshot)); err != nil {
		return err
	}
	g.lastSave = time.Now()
	return nil
}

func (s *Server) submitGeneration(ctx context.Context, chat storage.Chat, message string, opts generationOptions) (storage.Generation, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.closing {
		return storage.Generation{}, context.Canceled
	}
	// The registry reserves capacity under the same lock as worker startup;
	// failed submissions never create a durable turn or a waiting goroutine.
	if len(s.generations) >= maxConcurrentGenerations {
		return storage.Generation{}, errGenerationCapacity
	}
	if err := s.store.InterruptExpiredGenerations(ctx, time.Now()); err != nil {
		return storage.Generation{}, err
	}
	if opts.ImageOptions.Deployment != "" {
		image, err := s.store.LatestImage(ctx, chat.ID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return storage.Generation{}, err
		}
		opts.SourceImageID = image.ID
	}
	request, err := json.Marshal(opts)
	if err != nil {
		return storage.Generation{}, err
	}
	turn, err := s.store.CreateGeneration(ctx, chat.ID, message, string(request), time.Now().Add(generationTimeout+10*time.Second))
	if err != nil {
		return storage.Generation{}, err
	}
	if isUntitled(chat.Title) {
		if err := s.store.UpdateChatTitle(ctx, chat.ID, makeTitle(message)); err != nil {
			slog.Warn("save provisional title", "chat", chat.ID, "err", err)
		}
	} else if err := s.store.TouchChat(ctx, chat.ID); err != nil {
		slog.Warn("touch chat", "chat", chat.ID, "err", err)
	}
	s.startGenerationLocked(turn, opts)
	return turn, nil
}

// startGenerationLocked registers work before Close begins waiting. The durable
// claim, not this in-memory cancellation registry, authorizes provider launches.
func (s *Server) startGenerationLocked(turn storage.Generation, opts generationOptions) {
	ctx, cancel := context.WithTimeout(s.ctx, generationTimeout)
	s.generations[turn.ID] = generationJob{chatID: turn.ChatID, cancel: cancel}
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer cancel()
		defer func() {
			s.configMu.Lock()
			delete(s.generations, turn.ID)
			s.configMu.Unlock()
		}()
		claimed, err := s.store.ClaimGeneration(ctx, turn.ChatID, turn.ID)
		if err != nil || !claimed {
			if err != nil {
				slog.Error("claim generation", "turn", turn.ID, "err", err)
				s.interruptGeneration(turn.ID)
			}
			return
		}
		run := &generationStream{server: s, ctx: ctx, turn: turn, options: opts,
			events: make(map[string]string), state: storage.GenerationCompleted}
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("generation panic", "turn", turn.ID, "panic", recovered)
				run.state = storage.GenerationInterrupted
				run.failure = "generation worker panic"
			}
			s.finishGeneration(run)
		}()
		s.generateTurn(ctx, run, turn, opts)
	}()
}

func (s *Server) watchGenerations() {
	defer close(s.generationWatchDone)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.store.InterruptExpiredGenerations(s.ctx, now); err != nil && s.ctx.Err() == nil {
				slog.Error("expire abandoned generations", "err", err)
			}
		}
	}
}

func (s *Server) interruptGeneration(id int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	_, err := s.store.FinishGeneration(ctx, id, storage.GenerationInterrupted, "", "", "generation could not start", nil, nil)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		slog.Error("interrupt generation", "turn", id, "err", err)
	}
}

func (s *Server) finishGeneration(run *generationStream) {
	if run.snapshotError != nil && !errors.Is(run.snapshotError, context.Canceled) && !errors.Is(run.snapshotError, storage.ErrNotFound) {
		slog.Warn("generation snapshot update failed", "turn", run.turn.ID, "err", run.snapshotError)
	}
	// Terminal replay renders the committed Markdown, not a redundant HTML copy.
	delete(run.events, "token")
	snapshot, err := json.Marshal(run.events)
	if err != nil || len(snapshot) > maxSnapshotBytes {
		slog.Error("encode generation snapshot", "turn", run.turn.ID, "err", err)
		snapshot = nil // The terminal message remains the authoritative result.
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	var formatImage func(int64) string
	if run.image != nil {
		formatImage = func(id int64) string { return imageMarkdown(id, run.image.Prompt) }
	}
	_, err = s.store.FinishGeneration(ctx, run.turn.ID, run.state, run.content, string(snapshot), run.failure, run.image, formatImage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		slog.Error("persist generation outcome", "turn", run.turn.ID, "err", err)
		// A failed transaction never permits another provider launch.
		s.interruptGeneration(run.turn.ID)
	}
}

// handleGenerate only observes an explicitly identified turn. Reloads and
// duplicate connections never start a provider request.
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	chatID, err := parseID(r)
	turnID, turnErr := strconv.ParseInt(r.URL.Query().Get("turn"), 10, 64)
	if err != nil || turnErr != nil || turnID <= 0 {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetGeneration(r.Context(), chatID, turnID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			s.httpError(w, err)
		}
		return
	}
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, s.t("stream.not_supported"), http.StatusInternalServerError)
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	last := make(map[string]string)
	for {
		g, err := s.store.GetGeneration(r.Context(), chatID, turnID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				_ = sse.send("done", "") // Deleted chats have nothing left to replay.
			}
			return
		}
		current := make(map[string]string)
		if g.Snapshot != "" {
			if err := json.Unmarshal([]byte(g.Snapshot), &current); err != nil {
				slog.Error("decode generation snapshot", "turn", g.ID, "err", err)
				return
			}
		}
		if !g.Active() {
			content := g.Content
			if g.State == storage.GenerationInterrupted {
				content += "\n\n⚠ " + s.t("stream.recovered_interrupt")
			}
			current["token"] = renderMarkdownString(content)
			// Replaying an older turn must not restore an obsolete title/sidebar.
			chats, listErr := s.store.ListChats(r.Context())
			chat, chatErr := s.store.GetChat(r.Context(), chatID)
			if listErr == nil && chatErr == nil {
				current["title"] = s.renderString("title-update", struct {
					Title       string
					Chats       []storage.Chat
					CurrentChat *storage.Chat
				}{Title: chat.Title, Chats: chats, CurrentChat: &chat})
			}
		}
		for _, event := range []string{"tool", "token", "model", "usage", "title"} {
			data := current[event]
			if data == last[event] {
				continue
			}
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return
			}
			if err := sse.send(event, data); err != nil {
				return // Browser disconnection does not cancel the worker.
			}
			last[event] = data
		}
		if !g.Active() {
			_ = sse.send("done", "") // Terminal outcome is already durable.
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
