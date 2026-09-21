package storage

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryPreservesOriginalMessagesAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	chat, err := s.CreateChat(ctx, "Retry", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.CreateGeneration(ctx, chat, "Original question", `{"model":"original"}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishGeneration(ctx, original.ID, GenerationFailed, "Original failure", "", "failed", nil, nil); err != nil {
		t.Fatal(err)
	}
	later, err := s.CreateGeneration(ctx, chat, "Later question", "{}", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishGeneration(ctx, later.ID, GenerationCompleted, "Later answer", "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	retry, err := s.RetryGeneration(ctx, chat, original.ID, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != retry.ResponseID || retry.ID == original.ID || retry.UserMessageID != original.ID || retry.Request != original.Request {
		t.Fatalf("retry identity is incorrect: %+v", retry)
	}
	history, err := s.GenerationHistory(ctx, chat, retry.ID)
	if err != nil || len(history) != 1 || history[0].Content != "Original question" {
		t.Fatalf("retry used later history: %+v, %v", history, err)
	}
	if _, err := s.FinishGeneration(ctx, retry.ID, GenerationCompleted, "New answer", "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	messages, turns, err := s.Conversation(ctx, chat)
	if err != nil || len(messages) != 5 || len(turns) != 3 ||
		messages[1].Content != "Original failure" || messages[3].Content != "Later answer" ||
		messages[4].Content != "New answer" || !messages[4].Retried || messages[4].GenerationID != retry.ID {
		t.Fatalf("retry modified prior messages or inserted another question: %+v, %v", messages, err)
	}
	retryAgain, err := s.RetryGeneration(ctx, chat, retry.ID, time.Now().Add(time.Minute))
	if err != nil || retryAgain.UserMessageID != original.ID {
		t.Fatalf("retry chain lost its original question: %+v, %v", retryAgain, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.GetGeneration(ctx, chat, retryAgain.ID)
	if err != nil || interrupted.State != GenerationInterrupted || interrupted.UserMessageID != original.ID {
		t.Fatalf("restart lost retry linkage: %+v, %v", interrupted, err)
	}
	if err := s.DeleteChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGeneration(ctx, chat, retry.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry survived deletion of its chat: %v", err)
	}
}

func TestRetryConcurrentAdmissionAndChatScope(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	chat, err := s.CreateChat(ctx, "Retry", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.CreateGeneration(ctx, chat, "Original question", "{}", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryGeneration(ctx, chat, original.ID, time.Now().Add(time.Minute)); !errors.Is(err, ErrGenerationBusy) {
		t.Fatalf("active request was retried: %v", err)
	}
	if _, err := s.FinishGeneration(ctx, original.ID, GenerationCompleted, "Answer", "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryGeneration(ctx, chat+1, original.ID, time.Now().Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry ignored chat scope: %v", err)
	}
	var accepted atomic.Int64
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			_, err := s.RetryGeneration(ctx, chat, original.ID, time.Now().Add(time.Minute))
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrGenerationBusy) {
				t.Errorf("retry: %v", err)
			}
		})
	}
	callers.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d simultaneous retry jobs", accepted.Load())
	}
}
