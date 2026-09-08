package storage

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerationAtomicSubmissionClaimAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	chatID, err := s.CreateChat(ctx, "test", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, err := s.CreateGeneration(ctx, chatID, "question", `{}`, time.Now().Add(time.Minute))
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrGenerationBusy) {
				t.Errorf("submit: %v", err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("accepted %d simultaneous submissions", successes.Load())
	}
	turns, err := s.ListGenerations(ctx, chatID)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns = %+v, %v", turns, err)
	}
	g := turns[0]
	var claims atomic.Int64
	for range 12 {
		wg.Go(func() {
			ok, err := s.ClaimGeneration(ctx, chatID, g.ID)
			if err != nil {
				t.Error(err)
			}
			if ok {
				claims.Add(1)
			}
		})
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("claimed %d times", claims.Load())
	}
	if _, err := s.AddMessage(ctx, chatID, "user", "a later message"); err != nil {
		t.Fatal(err)
	}
	history, err := s.GenerationHistory(ctx, chatID, g.ID)
	if err != nil || len(history) != 1 || history[0].ID != g.ID || history[0].Content != "question" {
		t.Fatalf("history crossed the user turn: %+v, %v", history, err)
	}
	if _, err := s.FinishGeneration(ctx, g.ID, GenerationCompleted, "answer", `{}`, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishGeneration(ctx, g.ID, GenerationCompleted, "duplicate", `{}`, "", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate finish = %v", err)
	}
	messages, err := s.ListMessages(ctx, chatID)
	if err != nil || len(messages) != 3 || messages[1].ID != g.ResponseID || messages[1].Content != "answer" {
		t.Fatalf("response position changed: %+v, %v", messages, err)
	}
}

func TestGenerationRestartAndExpiryNeverReclaim(t *testing.T) {
	for _, state := range []string{GenerationPending, GenerationRunning} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			chatID, err := s.CreateChat(ctx, "test", "", "auto")
			if err != nil {
				t.Fatal(err)
			}
			g, err := s.CreateGeneration(ctx, chatID, "question", `{}`, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if state == GenerationRunning {
				if ok, err := s.ClaimGeneration(ctx, chatID, g.ID); err != nil || !ok {
					t.Fatalf("claim = %v, %v", ok, err)
				}
				if err := s.UpdateGeneration(ctx, g.ID, "partial response", `{"token":"partial"}`); err != nil {
					t.Fatal(err)
				}
			}
			path := s.path
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() }) // Close the reopened database.
			if err := s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := s.GetGeneration(ctx, chatID, g.ID)
			if err != nil || recovered.State != GenerationInterrupted {
				t.Fatalf("recovery = %+v, %v", recovered, err)
			}
			if ok, err := s.ClaimGeneration(ctx, chatID, g.ID); err != nil || ok {
				t.Fatalf("interrupted operation reclaimed: %v, %v", ok, err)
			}
			messages, err := s.ListMessages(ctx, chatID)
			if err != nil || len(messages) != 2 || messages[1].Content != recovered.Content {
				t.Fatalf("partial outcome lost: %+v, %v", messages, err)
			}
			next, err := s.CreateGeneration(ctx, chatID, "next", `{}`, time.Now().Add(-time.Second))
			if err != nil {
				t.Fatalf("recovered claim stayed busy: %v", err)
			}
			if err := s.InterruptExpiredGenerations(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			expired, err := s.GetGeneration(ctx, chatID, next.ID)
			if err != nil || expired.State != GenerationInterrupted {
				t.Fatalf("expiry = %+v, %v", expired, err)
			}
		})
	}
}

func TestGenerationImageCompletionIsAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	chatID, err := s.CreateChat(ctx, "test", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGeneration(ctx, chatID, "draw", `{}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	image := &Image{Prompt: "draw", MIME: "image/png", Data: []byte("image")}
	format := func(id int64) string { return fmt.Sprintf("![draw](/images/%d)", id) }
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_response BEFORE UPDATE ON messages
		BEGIN SELECT RAISE(ABORT, 'simulated response write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishGeneration(ctx, g.ID, GenerationCompleted, "", `{}`, "", image, format); err == nil {
		t.Fatal("completion ignored a response write failure")
	}
	if count, err := s.CountImages(ctx, chatID); err != nil || count != 0 {
		t.Fatalf("failed completion left an orphaned image: %d, %v", count, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_response`); err != nil {
		t.Fatal(err)
	}
	content, err := s.FinishGeneration(ctx, g.ID, GenerationCompleted, "", `{}`, "", image, format)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishGeneration(ctx, g.ID, GenerationCompleted, "", `{}`, "", image, format); !errors.Is(err, ErrNotFound) {
		t.Fatal("duplicate completion inserted an image", err)
	}
	count, err := s.CountImages(ctx, chatID)
	if err != nil || count != 1 {
		t.Fatalf("image count = %d, %v", count, err)
	}
	messages, err := s.ListMessages(ctx, chatID)
	if err != nil || len(messages) != 2 || messages[1].Content != content {
		t.Fatalf("image answer not committed atomically: %+v, %v", messages, err)
	}
	if err := s.DeleteChat(ctx, chatID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGeneration(ctx, chatID, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("generation survived chat deletion: %v", err)
	}
}
