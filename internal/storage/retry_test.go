package storage

import (
	"errors"
	"testing"
	"time"
)

func TestRetryQuestionThroughoutHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	chat, err := s.CreateChat(ctx, "Retry", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	add := func(role, content string) int64 {
		t.Helper()
		id, err := s.AddMessage(ctx, chat, role, content)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	orphan := add("assistant", "No question")
	legacyUser := add("user", "Legacy question")
	legacyAnswer := add("assistant", "Legacy answer")
	original, err := s.CreateGeneration(ctx, chat, "Original question", "{}", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishGeneration(ctx, original.ID, GenerationFailed, "Original failure", "", "failed", nil, nil); err != nil {
		t.Fatal(err)
	}
	laterUser := add("user", "Later question")
	laterAnswer := add("assistant", "Later answer")
	// Existing assistant-only retries must keep their explicit earlier question.
	oldRetry := add("assistant", "Old retry of original question")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO generations
		(id,chat_id,response_id,user_message_id,state,request,deadline,updated_at)
		VALUES (?,?,?,?,'completed','{}',?,?)`, oldRetry, chat, oldRetry, original.ID, nowStr(), nowStr()); err != nil {
		t.Fatal(err)
	}
	repeated, err := s.CreateGeneration(ctx, chat, "Original question", "{}", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	expected := map[int64]int64{
		legacyUser: legacyUser, legacyAnswer: legacyUser,
		original.ID: original.ID, original.ResponseID: original.ID,
		laterUser: laterUser, laterAnswer: laterUser, oldRetry: original.ID,
		repeated.ID: repeated.ID, repeated.ResponseID: repeated.ID,
	}
	check := func() {
		t.Helper()
		messages, _, err := s.Conversation(ctx, chat)
		if err != nil || len(messages) != 10 {
			t.Fatalf("conversation: %+v, %v", messages, err)
		}
		for _, message := range messages {
			if message.QuestionID != expected[message.ID] {
				t.Errorf("message %d question = %d, want %d", message.ID, message.QuestionID, expected[message.ID])
			}
			question, err := s.RetryQuestion(ctx, chat, message.ID)
			if message.ID == orphan {
				if !errors.Is(err, ErrNotFound) {
					t.Errorf("orphan answer resolved a question: %+v, %v", question, err)
				}
				continue
			}
			if err != nil || question.ID != expected[message.ID] || question.Role != "user" || question.Content == "" {
				t.Errorf("retry question for %d: %+v, %v", message.ID, question, err)
			}
		}
	}
	check()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	check()
	for _, scope := range [][2]int64{{chat + 1, original.ID}, {chat, oldRetry + 100}} {
		if _, err := s.RetryQuestion(ctx, scope[0], scope[1]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid retry scope %v: %v", scope, err)
		}
	}
	history, err := s.GenerationHistory(ctx, chat, repeated.ID)
	if err != nil || len(history) != 9 || history[len(history)-1].ID != repeated.ID {
		t.Fatalf("new submission did not include current history: %+v, %v", history, err)
	}
	if err := s.DeleteChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryQuestion(ctx, chat, oldRetry); !errors.Is(err, ErrNotFound) {
		t.Fatalf("question survived deletion of its chat: %v", err)
	}
}
