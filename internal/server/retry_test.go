package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daknoblo/ai-ui/internal/storage"
)

func retryRequest(t *testing.T, handler http.Handler, chatID, turnID int64) *httptest.ResponseRecorder {
	t.Helper()
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/chat/%d/retry/%d", chatID, turnID), nil))
	return result
}

func TestRetryUsesOriginalQuestionAndSavedOptions(t *testing.T) {
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
	retry := retryRequest(t, handler, id, turns[0].ID)
	retryURL := submittedGenerationURL(t, retry)
	if strings.Contains(retry.Body.String(), `class="msg user"`) || !strings.Contains(retry.Body.String(), s.t("retry.answer")) {
		t.Fatal("retry must append only a labeled assistant stream")
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
	if body.Model != "gpt-4o" || body.Messages[len(body.Messages)-1].Content != "Original question" {
		t.Fatalf("retry used changed options or latest question: %+v", body)
	}
	for _, message := range body.Messages {
		if strings.Contains(message.Content, "Later question") || strings.Contains(message.Content, "Answer 1") {
			t.Fatalf("retry context includes messages after the original request: %+v", body)
		}
	}
	messages, err := s.store.ListMessages(t.Context(), id)
	if err != nil || len(messages) != 7 || messages[3].Content != "Answer 1" ||
		messages[5].Content != "Answer 2" || messages[6].Content != "Answer 3" || calls.Load() != 3 {
		t.Fatalf("previous messages or call count changed: %+v, calls=%d, err=%v", messages, calls.Load(), err)
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/chat/%d", id), nil))
	if strings.Count(page.Body.String(), `class="response-retry"`) != 4 ||
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
	if err != nil || len(messages) != 5 || !strings.Contains(messages[3].Content, "temporary upstream failure") ||
		messages[4].Content != "Recovered answer" || calls.Load() != 2 {
		t.Fatalf("failed answer was replaced or retried more than once: %+v, %v", messages, err)
	}
}

func TestRetryImageEditKeepsOriginalSource(t *testing.T) {
	sources := make(chan string, 2)
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
	retryURL := submittedGenerationURL(t, retryRequest(t, handler, id, turns[0].ID))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, retryURL, nil))
	s.jobs.Wait()
	for range 2 {
		if source := <-sources; source != "original source" {
			t.Fatalf("retry substituted a newer image: %q", source)
		}
	}
	if err := s.store.DeleteImageForChat(t.Context(), id, sourceID); err != nil {
		t.Fatal(err)
	}
	if retry := retryRequest(t, handler, id, turns[0].ID); retry.Code != http.StatusConflict ||
		!strings.Contains(retry.Body.String(), s.t("stream.image_source_missing")) {
		t.Fatalf("missing original image silently fell back: %d %s", retry.Code, retry.Body.String())
	}
}
