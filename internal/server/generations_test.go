package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func submittedGenerationURL(t *testing.T, result *httptest.ResponseRecorder) string {
	t.Helper()
	if result.Code != http.StatusOK {
		t.Fatalf("send = %d: %s", result.Code, result.Body.String())
	}
	match := regexp.MustCompile(`sse-connect="([^"]+)"`).FindStringSubmatch(result.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing durable stream URL: %s", result.Body.String())
	}
	return html.UnescapeString(match[1])
}

func generationURL(chatID, turnID int64) string {
	return fmt.Sprintf("/chat/%d/generate?turn=%d", chatID, turnID)
}

func sendGeneration(t *testing.T, handler http.Handler, chatID int64, form string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/chat/%d/send", chatID), strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}

func generationTestServer(t *testing.T, endpoint string) (*Server, http.Handler, int64) {
	t.Helper()
	s, handler := newConfiguredServer(t, "en", config.Keys{API: "local-test"}, config.Overrides{},
		func(cfg *config.Config) {
			cfg.Endpoint = endpoint
			cfg.ChatDeployment = "gpt-4o"
			cfg.ChatModels = []string{"gpt-4o"}
			cfg.ImageEndpoint, cfg.ImageDeployment = "", ""
		})
	id, err := s.store.CreateChat(t.Context(), "Existing", "gpt-4o", "auto")
	if err != nil {
		t.Fatal(err)
	}
	// Avoid automatic title generation in tests that count answer requests.
	for _, message := range []struct{ role, content string }{{"user", "Earlier"}, {"assistant", "Earlier answer"}} {
		if _, err := s.store.AddMessage(t.Context(), id, message.role, message.content); err != nil {
			t.Fatal(err)
		}
	}
	return s, handler, id
}

func writeGenerationAnswer(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	data, _ := json.Marshal(map[string]any{"model": "gpt-4o", "choices": []any{
		map[string]any{"delta": map[string]string{"content": text}, "finish_reason": "stop"},
	}})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data) // Test clients may already have canceled.
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for generation")
	}
}

type generationObserver struct {
	*httptest.ResponseRecorder
	opened chan struct{}
	once   sync.Once
}

func (w *generationObserver) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.opened) })
}

func TestGenerationReconnectAndParallelSubmits(t *testing.T) {
	var calls atomic.Int64
	entered, release := make(chan struct{}), make(chan struct{})
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		writeGenerationAnswer(w, "One durable answer")
	}))
	t.Cleanup(azure.Close)
	s, handler, id := generationTestServer(t, azure.URL)
	first := sendGeneration(t, handler, id, "message=First")
	streamURL := submittedGenerationURL(t, first)
	waitSignal(t, entered)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
	if !strings.Contains(html.UnescapeString(page.Body.String()), streamURL) {
		t.Fatal("refresh did not recover the pending turn")
	}
	var submits sync.WaitGroup
	for range 8 {
		submits.Go(func() {
			got := sendGeneration(t, handler, id, "message=Must+not+append")
			if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), s.t("stream.busy")) {
				t.Errorf("parallel submit = %d: %s", got.Code, got.Body.String())
			}
		})
	}
	submits.Wait()
	legacy := httptest.NewRecorder()
	handler.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d/generate", id), nil))
	if legacy.Code != http.StatusNotFound {
		t.Fatal("legacy GET still guessed the latest user message")
	}
	wrongChat := httptest.NewRecorder()
	handler.ServeHTTP(wrongChat, httptest.NewRequest(http.MethodGet,
		strings.Replace(streamURL, fmt.Sprintf("/chat/%d/", id), fmt.Sprintf("/chat/%d/", id+1), 1), nil))
	if wrongChat.Code != http.StatusNotFound {
		t.Fatal("generation replay ignored its chat parent")
	}

	disconnected, cancel := context.WithCancel(t.Context())
	observer := &generationObserver{ResponseRecorder: httptest.NewRecorder(), opened: make(chan struct{})}
	observerDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(observer, httptest.NewRequest(http.MethodGet, streamURL, nil).WithContext(disconnected))
		close(observerDone)
	}()
	waitSignal(t, observer.opened)
	cancel()
	waitSignal(t, observerDone)
	var streams sync.WaitGroup
	for range 6 {
		streams.Go(func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, streamURL, nil))
			if !strings.Contains(response.Body.String(), "One durable answer") || !strings.Contains(response.Body.String(), "event: done") {
				t.Errorf("reconnect did not replay result: %s", response.Body.String())
			}
		})
	}
	close(release)
	streams.Wait()
	if calls.Load() != 1 {
		t.Fatalf("provider launched %d times", calls.Load())
	}
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil || len(messages) != 4 || messages[2].Content != "First" || messages[3].Content != "One durable answer" {
		t.Fatalf("turn messages = %+v, %v", messages, err)
	}
	secondURL := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Second"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, secondURL, nil))
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, streamURL, nil))
	if calls.Load() != 2 || !strings.Contains(replay.Body.String(), "One durable answer") {
		t.Fatal("replaying an older turn launched or attached to a newer operation")
	}
	page = httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
	if !strings.Contains(page.Body.String(), "One durable answer") || strings.Contains(page.Body.String(), "sse-connect=") {
		t.Fatal("refresh lost the terminal answer or reconnected a completed operation")
	}
}

func TestGenerationFailureIsDurableAndNotRetried(t *testing.T) {
	var calls atomic.Int64
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "uncertain provider failure", http.StatusBadGateway)
	}))
	t.Cleanup(azure.Close)
	s, handler, id := generationTestServer(t, azure.URL)
	cfg := s.cfg.Get()
	cfg.SearchAuto, cfg.SearchProvider, cfg.SearchEndpoint = true, "searxng", azure.URL
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	streamURL := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Fail"))
	for range 3 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, streamURL, nil))
		if !strings.Contains(response.Body.String(), "⚠") || !strings.Contains(response.Body.String(), "event: done") {
			t.Fatalf("failure lost: %s", response.Body.String())
		}
	}
	turns, err := s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 1 || turns[0].State != storage.GenerationFailed || calls.Load() != 1 {
		t.Fatalf("failure outcome = %+v, calls=%d, %v", turns, calls.Load(), err)
	}
	if next := sendGeneration(t, handler, id, "message=Explicit+new+turn"); next.Code != http.StatusOK {
		t.Fatal("failed generation left the chat busy")
	}
}

func TestGenerationCapacityAcrossChats(t *testing.T) {
	var calls atomic.Int64
	entered := make(chan struct{}, maxConcurrentGenerations*3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeGenerationAnswer(w, "Bounded answer")
	}))
	t.Cleanup(azure.Close)
	s, handler, firstID := generationTestServer(t, azure.URL)
	t.Cleanup(unblock)
	chatIDs := []int64{firstID}
	for len(chatIDs) < maxConcurrentGenerations*3 {
		id, err := s.store.CreateChat(t.Context(), "Existing", "gpt-4o", "auto")
		if err != nil {
			t.Fatal(err)
		}
		for _, role := range []string{"user", "assistant"} {
			if _, err := s.store.AddMessage(t.Context(), id, role, "Earlier"); err != nil {
				t.Fatal(err)
			}
		}
		chatIDs = append(chatIDs, id)
	}
	results := make([]*httptest.ResponseRecorder, len(chatIDs))
	var submits sync.WaitGroup
	for i, id := range chatIDs {
		submits.Go(func() { results[i] = sendGeneration(t, handler, id, "message=Bounded") })
	}
	submits.Wait()
	accepted, rejectedID := 0, int64(0)
	for i, result := range results {
		if result.Code == http.StatusOK {
			accepted++
			continue
		}
		if result.Code != http.StatusServiceUnavailable || result.Header().Get("Retry-After") == "" ||
			!strings.Contains(result.Body.String(), s.t("stream.overloaded")) {
			t.Fatalf("overload response = %d: %s", result.Code, result.Body.String())
		}
		rejectedID = chatIDs[i]
		messages, err := s.store.ListMessages(t.Context(), rejectedID)
		if err != nil || len(messages) != 2 {
			t.Fatalf("rejected submission stored a message: %+v, %v", messages, err)
		}
		turns, err := s.store.ListGenerations(t.Context(), rejectedID)
		if err != nil || len(turns) != 0 {
			t.Fatalf("rejected submission queued a turn: %+v, %v", turns, err)
		}
	}
	if accepted != maxConcurrentGenerations {
		t.Fatalf("accepted %d requests, want %d", accepted, maxConcurrentGenerations)
	}
	for range accepted {
		waitSignal(t, entered)
	}
	if calls.Load() != int64(maxConcurrentGenerations) {
		t.Fatalf("provider calls exceeded capacity: %d", calls.Load())
	}
	unblock()
	s.jobs.Wait()
	if next := sendGeneration(t, handler, rejectedID, "message=Explicit+retry"); next.Code != http.StatusOK {
		t.Fatalf("completed workers did not release capacity: %d, %s", next.Code, next.Body.String())
	}
	s.jobs.Wait()
	if calls.Load() != int64(maxConcurrentGenerations+1) {
		t.Fatalf("unexpected provider calls after retry: %d", calls.Load())
	}
}

func TestGenerationProviderHistoryEndsAtBoundUserMessage(t *testing.T) {
	requests := make(chan []map[string]any, 1)
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body.Messages
		writeGenerationAnswer(w, "Bound answer")
	}))
	t.Cleanup(azure.Close)
	s, handler, id := generationTestServer(t, azure.URL)
	opts := generationOptions{}
	encoded, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.store.CreateGeneration(t.Context(), id, "Bound question", string(encoded), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddMessage(t.Context(), id, "user", "Future question"); err != nil {
		t.Fatal(err)
	}
	s.configMu.Lock()
	s.startGenerationLocked(g, opts)
	s.configMu.Unlock()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, generationURL(id, g.ID), nil))
	messages := <-requests
	if len(messages) == 0 || messages[len(messages)-1]["content"] != "Bound question" {
		t.Fatalf("provider history = %+v", messages)
	}
	for _, message := range messages {
		if message["content"] == "Future question" {
			t.Fatal("generation borrowed history from a later message")
		}
	}
}

func TestGenerationRestartReplaysInterruptionWithoutLaunching(t *testing.T) {
	s, _ := newTestServer(t, "en")
	id, err := s.store.CreateChat(t.Context(), "Restart", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.store.CreateGeneration(t.Context(), id, "question", `{}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.store.ClaimGeneration(t.Context(), id, g.ID); err != nil || !ok {
		t.Fatal("claim failed", err)
	}
	if err := s.store.UpdateGeneration(t.Context(), g.ID, "Saved partial", `{}`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.cfg, s.store, s.logs)
	t.Cleanup(restarted.Close)
	response := httptest.NewRecorder()
	restarted.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, generationURL(id, g.ID), nil))
	if !strings.Contains(response.Body.String(), "Saved partial") || !strings.Contains(response.Body.String(), restarted.t("stream.recovered_interrupt")) ||
		!strings.Contains(response.Body.String(), "event: done") {
		t.Fatalf("restart outcome = %s", response.Body.String())
	}
	if ok, err := s.store.ClaimGeneration(t.Context(), id, g.ID); err != nil || ok {
		t.Fatal("restarted generation was reclaimed", err)
	}
}

func TestGenerationTitleAndProviderCancelOnShutdown(t *testing.T) {
	for _, title := range []bool{false, true} {
		t.Run(fmt.Sprintf("title=%v", title), func(t *testing.T) {
			var calls atomic.Int64
			blocked := make(chan struct{})
			azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if title && n == 1 {
					writeGenerationAnswer(w, "Completed answer")
					return
				}
				close(blocked)
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			t.Cleanup(azure.Close)
			s, handler, id := generationTestServer(t, azure.URL)
			if title {
				var err error
				id, err = s.store.CreateChat(t.Context(), "First exchange", "gpt-4o", "auto")
				if err != nil {
					t.Fatal(err)
				}
			}
			sendGeneration(t, handler, id, "message=Question")
			waitSignal(t, blocked)
			closed := make(chan struct{})
			go func() {
				s.Close()
				close(closed)
			}()
			waitSignal(t, closed)
			turns, err := s.store.ListGenerations(t.Context(), id)
			if err != nil || len(turns) != 1 || turns[0].Active() {
				t.Fatalf("shutdown left an active claim: %+v, %v", turns, err)
			}
			if title {
				messages, err := s.store.ListMessages(t.Context(), id)
				if err != nil || messages[len(messages)-1].Content != "Completed answer" {
					t.Fatalf("title cancellation discarded the completed answer: %+v, %v", messages, err)
				}
			}
		})
	}
}

func TestGenerationDeletionCancelsProvider(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(azure.Close)
	s, handler, id := generationTestServer(t, azure.URL)
	streamURL := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Question"))
	waitSignal(t, entered)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/chats/%d", id), nil))
	waitSignal(t, canceled)
	s.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, streamURL, nil))
	// Inspect persistence directly after shutdown; requests now have canceled contexts.
	if _, err := s.store.GetChat(t.Context(), id); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("deleted chat was resurrected by completion", err)
	}
}

func TestGenerationImageReplayDoesNotDuplicate(t *testing.T) {
	var calls atomic.Int64
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString([]byte("image"))}},
		}) // In-process test response.
	}))
	t.Cleanup(azure.Close)
	s, handler, id := generationTestServer(t, azure.URL)
	cfg := s.cfg.Get()
	cfg.ImageEndpoint, cfg.ImageDeployment = azure.URL, "gpt-image-1"
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	streamURL := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Draw+once&mode=image"))
	for range 3 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, streamURL+"&image=0", nil))
		if !strings.Contains(response.Body.String(), "/images/") {
			t.Fatalf("image missing: %s", response.Body.String())
		}
	}
	count, err := s.store.CountImages(t.Context(), id)
	if err != nil || count != 1 || calls.Load() != 1 {
		t.Fatalf("images=%d calls=%d err=%v", count, calls.Load(), err)
	}
}

func TestFoundryPerChatImageChoice(t *testing.T) {
	s, handler := newTestServer(t, "en")
	snapshot := foundry.Snapshot{ResourceID: serverResourceID, Endpoint: "https://local.openai.azure.com/openai/v1", Deployments: []foundry.Deployment{
		{Name: "text", ModelName: "gpt-4o", ModelFormat: "OpenAI", ProvisioningState: "Succeeded"},
		{Name: "art", ModelName: "gpt-image-2", ModelFormat: "OpenAI", ProvisioningState: "Succeeded"},
	}}
	s.cfg.ConfigureFoundry(serverResourceID, &serverFoundrySource{snapshot: snapshot}, nil)
	if err := s.cfg.SetCatalog(snapshot); err != nil {
		t.Fatal(err)
	}
	id, err := s.store.CreateChat(t.Context(), "Image", "text", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateChatMode(t.Context(), id, storage.ChatModeImage); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/chat/%d/model", id), strings.NewReader("model=art"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	chat, err := s.store.GetChat(t.Context(), id)
	if response.Code != http.StatusNoContent || err != nil || chat.ImageModel != "art" || chat.Model != "text" {
		t.Fatalf("image selection = %d, %+v, %v: %s", response.Code, chat, err, response.Body.String())
	}
}

func TestAttachmentDeletesRequireMatchingChat(t *testing.T) {
	s, handler := newTestServer(t, "en")
	first, err := s.store.CreateChat(t.Context(), "First", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.store.CreateChat(t.Context(), "Second", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.store.CreateDocument(t.Context(), first, "doc.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.store.AddImage(t.Context(), first, storage.ImageUpload, "pic.png", "", "image/png", []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []struct {
		kind string
		id   int64
	}{{"documents", doc}, {"images", image}} {
		for _, test := range []struct {
			chat int64
			code int
		}{{second, http.StatusNotFound}, {first, http.StatusOK}, {first, http.StatusNotFound}} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete,
				fmt.Sprintf("/chat/%d/%s/%d", test.chat, resource.kind, resource.id), nil))
			if response.Code != test.code {
				t.Errorf("%s parent %d: got %d, want %d", resource.kind, test.chat, response.Code, test.code)
			}
		}
	}
}
