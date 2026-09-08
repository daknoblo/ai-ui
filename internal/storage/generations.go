package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrGenerationBusy rejects overlapping turns before either message is inserted.
var ErrGenerationBusy = errors.New("a generation is already active in this chat")

const (
	GenerationPending     = "pending"
	GenerationRunning     = "running"
	GenerationCompleted   = "completed"
	GenerationFailed      = "failed"
	GenerationInterrupted = "interrupted"
)

// Generation identifies a durable turn, including its reserved response position.
// Request contains non-secret, immutable generation options; Snapshot holds only
// the latest bounded SSE view, not an event log.
type Generation struct {
	ID, ChatID, ResponseID int64
	State, Request         string
	Content, Snapshot      string
	Error                  string
}

func (g Generation) Active() bool {
	return g.State == GenerationPending || g.State == GenerationRunning
}

func (s *Store) migrateGenerations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS generations (
	id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
	chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
	response_id INTEGER NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
	state TEXT NOT NULL CHECK(state IN ('pending','running','completed','failed','interrupted')),
	request TEXT NOT NULL,
	content TEXT NOT NULL DEFAULT '',
	snapshot TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	deadline TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_generation_active ON generations(chat_id)
	WHERE state IN ('pending','running');
UPDATE messages SET content = (SELECT content FROM generations WHERE response_id = messages.id)
	WHERE id IN (SELECT response_id FROM generations WHERE state IN ('pending','running'));
UPDATE generations SET state = 'interrupted', error = 'server restarted', snapshot = ''
	WHERE state IN ('pending','running');`)
	return err
}

// CreateGeneration atomically stores both message positions and the pending
// operation. Its ID is the exact user message ID, never "the latest message".
func (s *Store) CreateGeneration(ctx context.Context, chatID int64, content, request string, deadline time.Time) (Generation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer func() { _ = tx.Rollback() }() // No-op after commit.
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM generations WHERE chat_id = ? AND state IN ('pending','running')`, chatID).Scan(&active); err != nil {
		return Generation{}, err
	}
	if active != 0 {
		return Generation{}, ErrGenerationBusy
	}
	user, err := tx.ExecContext(ctx, `INSERT INTO messages(chat_id,role,content,created_at) VALUES (?,'user',?,?)`, chatID, content, nowStr())
	if err != nil {
		return Generation{}, err
	}
	id, err := user.LastInsertId()
	if err != nil {
		return Generation{}, err
	}
	answer, err := tx.ExecContext(ctx, `INSERT INTO messages(chat_id,role,content,created_at) VALUES (?,'assistant','',?)`, chatID, nowStr())
	if err != nil {
		return Generation{}, err
	}
	responseID, err := answer.LastInsertId()
	if err != nil {
		return Generation{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO generations(id,chat_id,response_id,state,request,deadline,updated_at) VALUES (?,?,?,'pending',?,?,?)`,
		id, chatID, responseID, request, deadline.UTC().Format(timeLayout), nowStr())
	if err != nil {
		return Generation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Generation{}, err
	}
	return Generation{ID: id, ChatID: chatID, ResponseID: responseID, State: GenerationPending, Request: request}, nil
}

// ClaimGeneration is the only transition that grants permission to call a
// provider. Running, failed and interrupted operations can never be reclaimed.
func (s *Store) ClaimGeneration(ctx context.Context, chatID, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE generations SET state='running',updated_at=?
		WHERE id=? AND chat_id=? AND state='pending' AND deadline>?`, nowStr(), id, chatID, nowStr())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) GetGeneration(ctx context.Context, chatID, id int64) (Generation, error) {
	var g Generation
	err := s.db.QueryRowContext(ctx, `SELECT id,chat_id,response_id,state,request,content,snapshot,error
		FROM generations WHERE chat_id=? AND id=?`, chatID, id).
		Scan(&g.ID, &g.ChatID, &g.ResponseID, &g.State, &g.Request, &g.Content, &g.Snapshot, &g.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

// ListGenerations omits the replay snapshots when constructing a chat page.
func (s *Store) ListGenerations(ctx context.Context, chatID int64) ([]Generation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,chat_id,response_id,state,request FROM generations WHERE chat_id=? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // Read-only cursor cleanup.
	var out []Generation
	for rows.Next() {
		var g Generation
		if err := rows.Scan(&g.ID, &g.ChatID, &g.ResponseID, &g.State, &g.Request); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Conversation reads message positions and generation states from one SQLite
// snapshot, so refreshing during a completion cannot render an empty response.
func (s *Store) Conversation(ctx context.Context, chatID int64) ([]Message, []Generation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.chat_id,m.role,m.content,m.created_at,
		COALESCE(g.id,0),COALESCE(g.state,''),COALESCE(g.request,'')
		FROM messages m LEFT JOIN generations g ON g.response_id=m.id WHERE m.chat_id=? ORDER BY m.id`, chatID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }() // Read-only cursor cleanup.
	var messages []Message
	var turns []Generation
	for rows.Next() {
		var m Message
		var g Generation
		var created string
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &created, &g.ID, &g.State, &g.Request); err != nil {
			return nil, nil, err
		}
		m.CreatedAt = parseTime(created)
		messages = append(messages, m)
		if g.ID != 0 {
			g.ChatID, g.ResponseID = chatID, m.ID
			turns = append(turns, g)
		}
	}
	return messages, turns, rows.Err()
}

// GenerationHistory excludes the reserved response and every later turn.
func (s *Store) GenerationHistory(ctx context.Context, chatID, id int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,chat_id,role,content,created_at FROM messages
		WHERE chat_id=? AND id<=? AND EXISTS (SELECT 1 FROM generations WHERE id=? AND chat_id=?)
		ORDER BY id`, chatID, id, id, chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // Read-only cursor cleanup.
	var out []Message
	for rows.Next() {
		var m Message
		var created string
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpdateGeneration(ctx context.Context, id int64, content, snapshot string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE generations SET content=?,snapshot=?,updated_at=? WHERE id=? AND state='running'`,
		content, snapshot, nowStr(), id)
	if err != nil {
		return err
	}
	return requireChanged(res)
}

// FinishGeneration persists the response and outcome in one transaction. When
// an image is supplied, its blob and Markdown are committed in that transaction
// too. imageContent must be a pure formatter with no database access.
func (s *Store) FinishGeneration(ctx context.Context, id int64, state, content, snapshot, failure string, image *Image, imageContent func(int64) string) (string, error) {
	if state != GenerationCompleted && state != GenerationFailed && state != GenerationInterrupted {
		return "", fmt.Errorf("invalid terminal generation state: %s", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }() // No-op after commit.
	var chatID, responseID int64
	err = tx.QueryRowContext(ctx, `SELECT chat_id,response_id FROM generations WHERE id=? AND state IN ('pending','running')`, id).Scan(&chatID, &responseID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if image != nil {
		if imageContent == nil || state != GenerationCompleted {
			return "", errors.New("image completion requires a formatter and successful outcome")
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO images(chat_id,kind,name,prompt,mime,data,created_at) VALUES (?,'generated',?,?,?,?,?)`,
			chatID, image.Name, image.Prompt, image.MIME, image.Data, nowStr())
		if err != nil {
			return "", err
		}
		imageID, err := res.LastInsertId()
		if err != nil {
			return "", err
		}
		content = imageContent(imageID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET content=? WHERE id=? AND chat_id=?`, content, responseID, chatID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE generations SET state=?,content=?,snapshot=?,error=?,updated_at=? WHERE id=?`,
		state, content, snapshot, failure, nowStr(), id); err != nil {
		return "", err
	}
	return content, tx.Commit()
}

// InterruptExpiredGenerations releases abandoned pending/running turns without
// retrying a provider call whose outcome is unknown.
func (s *Store) InterruptExpiredGenerations(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // No-op after commit.
	at := now.UTC().Format(timeLayout)
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET content=(SELECT content FROM generations WHERE response_id=messages.id)
		WHERE id IN (SELECT response_id FROM generations WHERE state IN ('pending','running') AND deadline<=?)`, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE generations SET state='interrupted',snapshot='',error='generation deadline exceeded',updated_at=?
		WHERE state IN ('pending','running') AND deadline<=?`, at, at); err != nil {
		return err
	}
	return tx.Commit()
}

func requireChanged(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
