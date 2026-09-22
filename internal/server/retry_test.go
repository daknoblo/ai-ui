package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func retryRequest(t *testing.T, handler http.Handler, chatID, turnID int64, form ...string) *httptest.ResponseRecorder {
	t.Helper()
	result := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/chat/%d/retry/%d", chatID, turnID), strings.NewReader(strings.Join(form, "&")))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(result, request)
	return result
}

func TestRetrySendsOriginalQuestionWithCurrentHistoryAndOptions(t *testing.T) {
	type request struct {
		Model    string `json:"model"`
		Messages []struct {
			Role, Content string
		} `json:"messages"`
	}
	requests := make(chan request, 3)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		number := calls.Add(1)
		if number == 3 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		writeGenerationAnswer(w, fmt.Sprintf("Answer %d", number))
	}))
	t.Cleanup(backend.Close)
	s, handler, id := generationTestServer(t, backend.URL)
	first := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Original+question"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, first, nil))
	second := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Later+question"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, second, nil))
	s.jobs.Wait()
	turns, err := s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 2 {
		t.Fatalf("turns: %+v, %v", turns, err)
	}
	if err := s.store.UpdateChatModel(t.Context(), id, "changed-model"); err != nil {
		t.Fatal(err)
	}
	retry := retryRequest(t, handler, id, turns[0].ResponseID)
	retryURL := submittedGenerationURL(t, retry)
	if !strings.Contains(retry.Body.String(), `class="msg user"`) || strings.Contains(retry.Body.String(), `class="retry-note"`) {
		t.Fatal("retry must append a normal user message and assistant stream")
	}
	waitSignal(t, entered)
	if duplicate := retryRequest(t, handler, id, turns[0].ID); duplicate.Code != http.StatusConflict {
		t.Fatalf("simultaneous retry = %d", duplicate.Code)
	}
	if wrong := retryRequest(t, handler, id+1, turns[0].ID); wrong.Code != http.StatusNotFound {
		t.Fatalf("wrong-chat retry = %d", wrong.Code)
	}
	close(release)
	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, retryURL, nil))
		if !strings.Contains(response.Body.String(), "Answer 3") {
			t.Fatal("retry result was not replayable")
		}
	}
	<-requests
	<-requests
	body := <-requests
	if body.Model != "changed-model" || body.Messages[len(body.Messages)-1].Content != "Original question" {
		t.Fatalf("retry did not use the current model and original question: %+v", body)
	}
	seen := make(map[string]bool)
	for _, message := range body.Messages {
		seen[message.Content] = true
	}
	for _, content := range []string{"Later question", "Answer 1", "Answer 2"} {
		if !seen[content] {
			t.Errorf("retry context omitted %q", content)
		}
	}
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil || len(messages) != 8 || messages[3].Content != "Answer 1" ||
		messages[5].Content != "Answer 2" || messages[6].Content != "Original question" ||
		messages[6].Role != "user" || messages[7].Content != "Answer 3" || calls.Load() != 3 {
		t.Fatalf("previous messages or call count changed: %+v, calls=%d, err=%v", messages, calls.Load(), err)
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
	if strings.Count(page.Body.String(), `class="response-retry"`) != 8 ||
		strings.Count(page.Body.String(), `class="response-copy"`) != 8 ||
		!strings.Contains(page.Body.String(), fmt.Sprintf(`hx-post="/chat/%d/retry/%d"`, id, turns[0].ID)) {
		t.Fatal("reloaded responses lost their retry actions")
	}
}

func TestRetryFailedRequestCanSucceed(t *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusServiceUnavailable)
			return
		}
		writeGenerationAnswer(w, "Recovered answer")
	}))
	t.Cleanup(backend.Close)
	s, handler, id := generationTestServer(t, backend.URL)
	first := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Question"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, first, nil))
	s.jobs.Wait()
	turns, err := s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 1 || turns[0].State != storage.GenerationFailed {
		t.Fatalf("expected a durable failure: %+v, %v", turns, err)
	}
	retryURL := submittedGenerationURL(t, retryRequest(t, handler, id, turns[0].ID))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, retryURL, nil))
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil || len(messages) != 6 || !strings.Contains(messages[3].Content, "temporary upstream failure") ||
		messages[4].Content != "Question" || messages[5].Content != "Recovered answer" || calls.Load() != 2 {
		t.Fatalf("failed answer was replaced or retried more than once: %+v, %v", messages, err)
	}
}

func TestRetryImageEditUsesCurrentSourceAndOptions(t *testing.T) {
	sources := make(chan string, 2)
	models := make(chan string, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/images/edits") {
			t.Errorf("unexpected route: %s", r.URL)
			http.Error(w, "unexpected route", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("image")
		if err != nil {
			t.Error(err)
			return
		}
		raw, err := io.ReadAll(file)
		_ = file.Close() // Local test fixture.
		if err != nil {
			t.Error(err)
			return
		}
		sources <- string(raw)
		models <- r.URL.Path
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}]}`) // Local image fixture.
	}))
	t.Cleanup(backend.Close)
	s, handler, id := generationTestServer(t, backend.URL)
	cfg := s.cfg.Get()
	cfg.ImageDeployment = "image-deployment"
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	sourceID, err := s.store.AddImage(t.Context(), id, storage.ImageUpload, "source.png", "", "image/png", []byte("original source"))
	if err != nil {
		t.Fatal(err)
	}
	first := submittedGenerationURL(t, sendGeneration(t, handler, id, "message=Edit+the+source&mode=image&edit=1"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, first, nil))
	s.jobs.Wait()
	turns, err := s.store.ListGenerations(t.Context(), id)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns: %+v, %v", turns, err)
	}
	if _, err := s.store.AddImage(t.Context(), id, storage.ImageUpload, "new.png", "", "image/png", []byte("new source")); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DeleteImageForChat(t.Context(), id, sourceID); err != nil {
		t.Fatal(err)
	}
	cfg.ImageDeployment = "new-image-deployment"
	if err := s.cfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	retryURL := submittedGenerationURL(t, retryRequest(t, handler, id, turns[0].ID, "mode=image&edit=1"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, retryURL, nil))
	s.jobs.Wait()
	for _, expected := range []string{"original source", "new source"} {
		if source := <-sources; source != expected {
			t.Fatalf("image source = %q, want %q", source, expected)
		}
	}
	for _, expected := range []string{"image-deployment", "new-image-deployment"} {
		if path := <-models; !strings.Contains(path, "/deployments/"+expected+"/") {
			t.Fatalf("image path = %q, want deployment %q", path, expected)
		}
	}
}

func TestRetryLegacyInputsAndOutputs(t *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Messages[len(body.Messages)-1].Content != "Earlier" {
			t.Errorf("legacy retry sent the answer instead of its question: %+v", body)
		}
		calls.Add(1)
		writeGenerationAnswer(w, "New answer")
	}))
	t.Cleanup(backend.Close)
	s, handler, id := generationTestServer(t, backend.URL)
	original, err := s.store.ListMessages(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range original {
		url := submittedGenerationURL(t, retryRequest(t, handler, id, message.ID))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, url, nil))
		s.jobs.Wait()
	}
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil || len(messages) != 6 || messages[2].Content != "Earlier" ||
		messages[4].Content != "Earlier" || messages[1].Content != "Earlier answer" || calls.Load() != 2 {
		t.Fatalf("legacy retry did not append new pairs: %+v, %v", messages, err)
	}
}

func TestRetryConcurrentSubmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeGenerationAnswer(w, "Answer")
	}))
	t.Cleanup(backend.Close)
	s, handler, id := generationTestServer(t, backend.URL)
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			response := retryRequest(t, handler, id, messages[0].ID)
			if response.Code == http.StatusOK {
				accepted.Add(1)
			} else if response.Code != http.StatusConflict {
				t.Errorf("retry status = %d", response.Code)
			}
		})
	}
	callers.Wait()
	waitSignal(t, entered)
	close(release)
	s.jobs.Wait()
	stored, err := s.store.ListMessages(t.Context(), id)
	if err != nil || accepted.Load() != 1 || len(stored) != 4 {
		t.Fatalf("overlapping retries created duplicate turns: accepted=%d messages=%+v err=%v", accepted.Load(), stored, err)
	}
}

func TestRetryUnavailableQuestionAndBodyLimit(t *testing.T) {
	s, handler, id := generationTestServer(t, "http://127.0.0.1:1")
	orphanChat, err := s.store.CreateChat(t.Context(), "Orphan", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.store.AddMessage(t.Context(), orphanChat, "assistant", "Answer without a question")
	if err != nil {
		t.Fatal(err)
	}
	if result := retryRequest(t, handler, orphanChat, orphan); result.Code != http.StatusNotFound ||
		!strings.Contains(result.Body.String(), s.t("retry.unavailable")) {
		t.Fatalf("orphan retry = %d: %s", result.Code, result.Body.String())
	}
	if result := retryRequest(t, handler, id, orphan); result.Code != http.StatusNotFound {
		t.Fatalf("cross-chat retry = %d", result.Code)
	}
	if result := retryRequest(t, handler, id, 1, "mode="+strings.Repeat("x", 256<<10)); result.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized retry = %d", result.Code)
	}
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil || len(messages) != 2 {
		t.Fatalf("rejected retry changed messages: %+v, %v", messages, err)
	}
}
