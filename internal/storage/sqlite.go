// Package storage wraps all access to the SQLite database.
package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store encapsulates access to the SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) the SQLite database at the given path.
//
// Pragmas:
//   - busy_timeout    waits instead of failing immediately on a locked database
//   - journal_mode    WAL keeps readers from blocking the single writer
//   - synchronous     NORMAL is the recommended companion of WAL: it avoids an
//     fsync per commit (far less I/O) and is still crash safe;
//     only a power loss can lose the most recent transactions
//   - foreign_keys    enables ON DELETE CASCADE
//   - auto_vacuum     INCREMENTAL lets deletes reclaim space without rewriting
//     the whole file (see Vacuum)
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=auto_vacuum(incremental)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite tolerates only a single writer, and this application is sized for a
	// handful of concurrent users. One connection keeps the pragmas above in
	// effect for every statement and removes all lock contention.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DiskUsage returns the current size of the database in bytes, including the
// WAL and SHM helper files that SQLite creates in WAL mode.
func (s *Store) DiskUsage() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Ping checks whether the database is reachable and writable.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	// Verify writability (is the data path mounted and writable?).
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _healthcheck (id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	return nil
}

// Migrate creates the schema if it does not exist yet.
func (s *Store) Migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS chats (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	title      TEXT NOT NULL,
	model      TEXT NOT NULL DEFAULT '',
	mode       TEXT NOT NULL DEFAULT 'chat',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chats_updated ON chats(updated_at DESC);
CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	created_at TEXT NOT NULL
);
-- Composite index: ListMessages filters by chat_id and orders by id, so this
-- serves both parts and removes the sort step.
CREATE INDEX IF NOT EXISTS idx_messages_chat_id ON messages(chat_id, id);
CREATE TABLE IF NOT EXISTS documents (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER REFERENCES chats(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	mime       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_documents_chat ON documents(chat_id);
CREATE TABLE IF NOT EXISTS chunks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	ordinal     INTEGER NOT NULL,
	text        TEXT NOT NULL,
	embedding   BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(document_id);
CREATE TABLE IF NOT EXISTS images (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL DEFAULT 'generated',
	name       TEXT NOT NULL DEFAULT '',
	prompt     TEXT NOT NULL,
	mime       TEXT NOT NULL,
	data       BLOB NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_images_chat ON images(chat_id);
CREATE TABLE IF NOT EXISTS usage_daily (
	day               TEXT NOT NULL,
	kind              TEXT NOT NULL,
	model             TEXT NOT NULL,
	requests          INTEGER NOT NULL,
	prompt_tokens     INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	total_tokens      INTEGER NOT NULL,
	PRIMARY KEY (day, kind, model)
);
-- UsageByModel filters on kind and groups by model.
CREATE INDEX IF NOT EXISTS idx_usage_kind_model ON usage_daily(kind, model);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Retrofit existing databases: add columns that were introduced later.
	if err := s.ensureColumn(ctx, `PRAGMA table_info(documents)`, "chat_id",
		`ALTER TABLE documents ADD COLUMN chat_id INTEGER REFERENCES chats(id) ON DELETE CASCADE`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, `PRAGMA table_info(chats)`, "model",
		`ALTER TABLE chats ADD COLUMN model TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, `PRAGMA table_info(chats)`, "mode",
		`ALTER TABLE chats ADD COLUMN mode TEXT NOT NULL DEFAULT 'chat'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, `PRAGMA table_info(images)`, "kind",
		`ALTER TABLE images ADD COLUMN kind TEXT NOT NULL DEFAULT 'generated'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, `PRAGMA table_info(images)`, "name",
		`ALTER TABLE images ADD COLUMN name TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// Older databases were created without auto_vacuum. The connection pragma
	// only takes effect for the current file after a full VACUUM, which is run
	// exactly once here.
	return s.ensureIncrementalVacuum(ctx)
}

// ensureIncrementalVacuum converts a legacy database to incremental auto-vacuum.
func (s *Store) ensureIncrementalVacuum(ctx context.Context) error {
	const incremental = 2
	var mode int
	if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if mode == incremental {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// ensureColumn adds a column when an older schema does not have it yet. pragma
// and alter are constant statements from the caller, never user input.
func (s *Store) ensureColumn(ctx context.Context, pragma, column, alter string) error {
	rows, err := s.db.QueryContext(ctx, pragma)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	found := false
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, alter)
	return err
}

// ---- Chats ----

// CreateChat creates a new chat and returns its ID. model pins the chat to a
// specific model; an empty value leaves the choice to the router.
func (s *Store) CreateChat(ctx context.Context, title, model string) (int64, error) {
	now := nowStr()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO chats (title, model, mode, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		title, model, ChatModeChat, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListChats returns all chats, most recently updated first.
func (s *Store) ListChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, model, mode, created_at, updated_at FROM chats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var chats []Chat
	for rows.Next() {
		var c Chat
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Title, &c.Model, &c.Mode, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(created)
		c.UpdatedAt = parseTime(updated)
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// GetChat returns a single chat.
func (s *Store) GetChat(ctx context.Context, id int64) (Chat, error) {
	var c Chat
	var created, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, model, mode, created_at, updated_at FROM chats WHERE id = ?`, id).
		Scan(&c.ID, &c.Title, &c.Model, &c.Mode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return c, nil
}

// UpdateChatModel pins a chat to a model (empty = router decides).
func (s *Store) UpdateChatModel(ctx context.Context, id int64, model string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chats SET model = ? WHERE id = ?`, model, id)
	return err
}

// UpdateChatMode stores the answer mode of a chat.
func (s *Store) UpdateChatMode(ctx context.Context, id int64, mode string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chats SET mode = ? WHERE id = ?`, mode, id)
	return err
}

// UpdateChatTitle changes the title of a chat.
func (s *Store) UpdateChatTitle(ctx context.Context, id int64, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chats SET title = ?, updated_at = ? WHERE id = ?`, title, nowStr(), id)
	return err
}

// TouchChat refreshes the updated_at timestamp.
func (s *Store) TouchChat(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chats SET updated_at = ? WHERE id = ?`, nowStr(), id)
	return err
}

// DeleteChat removes a chat including its messages and documents.
func (s *Store) DeleteChat(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chats WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Vacuum(ctx)
}

// DeleteEmptyChats removes chats that contain neither messages nor documents
// (orphaned "new chat" entries). exceptID is kept (0 = keep none). It returns
// the number of removed chats.
func (s *Store) DeleteEmptyChats(ctx context.Context, exceptID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chats
		 WHERE id != ?
		   AND id NOT IN (SELECT DISTINCT chat_id FROM messages)
		   AND id NOT IN (SELECT chat_id FROM documents WHERE chat_id IS NOT NULL)`,
		exceptID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- Messages ----

// AddMessage stores a message and returns its ID.
func (s *Store) AddMessage(ctx context.Context, chatID int64, role, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (chat_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		chatID, role, content, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListMessages returns all messages of a chat in chronological order.
func (s *Store) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, chat_id, role, content, created_at FROM messages WHERE chat_id = ? ORDER BY id ASC`,
		chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []Message
	for rows.Next() {
		var m Message
		var created string
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ---- Documents & chunks ----

// CreateDocument creates a document for a chat and returns its ID.
func (s *Store) CreateDocument(ctx context.Context, chatID int64, name, mime string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO documents (chat_id, name, mime, created_at) VALUES (?, ?, ?, ?)`,
		chatID, name, mime, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddChunks stores all sections of a document in a single transaction. One
// commit instead of one per chunk avoids a WAL sync per insert.
func (s *Store) AddChunks(ctx context.Context, documentID int64, texts []string, embeddings [][]float32) error {
	if len(texts) != len(embeddings) {
		return fmt.Errorf("chunk/embedding count mismatch: %d vs %d", len(texts), len(embeddings))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeded

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks (document_id, ordinal, text, embedding) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for i, text := range texts {
		if _, err := stmt.ExecContext(ctx, documentID, i, text, encodeEmbedding(embeddings[i])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDocumentsByChat returns the documents of a chat including the chunk count.
func (s *Store) ListDocumentsByChat(ctx context.Context, chatID int64) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.chat_id, d.name, d.mime, d.created_at, COUNT(c.id)
		 FROM documents d LEFT JOIN chunks c ON c.document_id = d.id
		 WHERE d.chat_id = ?
		 GROUP BY d.id ORDER BY d.created_at DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var docs []Document
	for rows.Next() {
		var d Document
		var created string
		if err := rows.Scan(&d.ID, &d.ChatID, &d.Name, &d.MIME, &created, &d.Chunks); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(created)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// DeleteDocument removes a document including its chunks.
func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Vacuum(ctx)
}

// Vacuum returns space freed by deletions to the file system.
//
// With auto_vacuum=INCREMENTAL (see Open) this only releases the free pages
// instead of rewriting the entire database, which keeps deletes cheap even for
// large embedding tables. The subsequent TRUNCATE checkpoint moves the change
// from the WAL into the main file so the size on disk actually shrinks.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	return nil
}

// EachChunkVector streams the embedding of every chunk that belongs to the
// documents of a chat.
//
// Retrieval only needs the vectors to score candidates, so the (much larger)
// chunk text is deliberately not loaded here; the texts of the few selected
// hits are fetched afterwards via ChunkTexts. The Embedding slice handed to fn
// is reused between rows and must not be retained by the callback.
func (s *Store) EachChunkVector(ctx context.Context, chatID int64, fn func(ChunkVector) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.document_id, c.embedding
		 FROM chunks c JOIN documents d ON d.id = c.document_id
		 WHERE d.chat_id = ?`, chatID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var (
		// sql.RawBytes avoids copying every BLOB; it stays valid until the next
		// call to rows.Next, which is why it is decoded immediately below.
		blob sql.RawBytes
		vec  []float32
		cv   ChunkVector
	)
	for rows.Next() {
		if err := rows.Scan(&cv.ID, &cv.DocumentID, &blob); err != nil {
			return err
		}
		vec = decodeEmbeddingInto(vec, blob)
		cv.Embedding = vec
		if err := fn(cv); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ChunkTexts loads the text of the given chunk IDs.
func (s *Store) ChunkTexts(ctx context.Context, ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	// Only the number of placeholders is derived from the input; the IDs
	// themselves are always passed as bound parameters.
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := `SELECT id, text FROM chunks WHERE id IN (?` + //#nosec G202 -- only "?" placeholders are concatenated, never user input
		strings.Repeat(",?", len(ids)-1) + `)`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var (
			id   int64
			text string
		)
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		out[id] = text
	}
	return out, rows.Err()
}

// CountChunksByChat returns the number of chunks across the documents of a chat.
func (s *Store) CountChunksByChat(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks c JOIN documents d ON d.id = c.document_id WHERE d.chat_id = ?`,
		chatID).Scan(&n)
	return n, err
}

// CountDocuments returns the total number of stored documents.
func (s *Store) CountDocuments(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents`).Scan(&n)
	return n, err
}

// ---- Generated images ----

// AddImage stores an image of a chat and returns its ID.
func (s *Store) AddImage(ctx context.Context, chatID int64, kind, name, prompt, mime string, data []byte) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO images (chat_id, kind, name, prompt, mime, data, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		chatID, kind, name, prompt, mime, data, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetImage loads an image by ID.
func (s *Store) GetImage(ctx context.Context, id int64) (Image, error) {
	var (
		img     Image
		created string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, chat_id, kind, name, prompt, mime, data, created_at FROM images WHERE id = ?`, id).
		Scan(&img.ID, &img.ChatID, &img.Kind, &img.Name, &img.Prompt, &img.MIME, &img.Data, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, err
	}
	img.CreatedAt = parseTime(created)
	return img, nil
}

// LatestImageByKind returns the most recent image of a kind, including its data.
func (s *Store) LatestImageByKind(ctx context.Context, chatID int64, kind string) (Image, error) {
	var (
		img     Image
		created string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, chat_id, kind, name, prompt, mime, data, created_at
		 FROM images WHERE chat_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, chatID, kind).
		Scan(&img.ID, &img.ChatID, &img.Kind, &img.Name, &img.Prompt, &img.MIME, &img.Data, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, err
	}
	img.CreatedAt = parseTime(created)
	return img, nil
}

// LatestImage returns the most recent image of a chat including its data.
func (s *Store) LatestImage(ctx context.Context, chatID int64) (Image, error) {
	var (
		img     Image
		created string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, chat_id, kind, name, prompt, mime, data, created_at
		 FROM images WHERE chat_id = ? ORDER BY id DESC LIMIT 1`, chatID).
		Scan(&img.ID, &img.ChatID, &img.Kind, &img.Name, &img.Prompt, &img.MIME, &img.Data, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, err
	}
	img.CreatedAt = parseTime(created)
	return img, nil
}

// CountImages returns how many images a chat holds.
func (s *Store) CountImages(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM images WHERE chat_id = ?`, chatID).Scan(&n)
	return n, err
}

// ListImagesByKind returns the images of a chat without their payload.
func (s *Store) ListImagesByKind(ctx context.Context, chatID int64, kind string) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, chat_id, kind, name, mime, created_at
		 FROM images WHERE chat_id = ? AND kind = ? ORDER BY id ASC`, chatID, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Image
	for rows.Next() {
		var (
			img     Image
			created string
		)
		if err := rows.Scan(&img.ID, &img.ChatID, &img.Kind, &img.Name, &img.MIME, &created); err != nil {
			return nil, err
		}
		img.CreatedAt = parseTime(created)
		out = append(out, img)
	}
	return out, rows.Err()
}

// DeleteImage removes a stored image.
func (s *Store) DeleteImage(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Vacuum(ctx)
}

// ---- Token usage (persistent statistics) ----

// RecordUsage books the token usage of a request, aggregated per day.
// kind is e.g. "chat" or "embedding"; model may be empty.
func (s *Store) RecordUsage(ctx context.Context, kind, model string, prompt, completion, total int) error {
	day := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_daily (day, kind, model, requests, prompt_tokens, completion_tokens, total_tokens)
		 VALUES (?, ?, ?, 1, ?, ?, ?)
		 ON CONFLICT(day, kind, model) DO UPDATE SET
		   requests          = requests + 1,
		   prompt_tokens     = prompt_tokens + excluded.prompt_tokens,
		   completion_tokens = completion_tokens + excluded.completion_tokens,
		   total_tokens      = total_tokens + excluded.total_tokens`,
		day, kind, model, prompt, completion, total)
	return err
}

// UsageSummary is the overall token usage overview.
type UsageSummary struct {
	ChatRequests     int64
	ChatPromptTokens int64
	ChatComplTokens  int64
	ChatTotalTokens  int64
	EmbedRequests    int64
	EmbedTotalTokens int64
	ImageRequests    int64
	ImageTotalTokens int64
	TotalTokens      int64
}

// UsageDay is the usage of a single day.
type UsageDay struct {
	Day             string
	ChatTokens      int64
	EmbeddingTokens int64
	TotalTokens     int64
	Requests        int64
}

// UsageModel is the usage per model.
type UsageModel struct {
	Model       string
	Requests    int64
	TotalTokens int64
}

// UsageSummaryTotals returns the aggregated overview.
func (s *Store) UsageSummaryTotals(ctx context.Context) (UsageSummary, error) {
	var u UsageSummary
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, SUM(requests), SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens)
		 FROM usage_daily GROUP BY kind`)
	if err != nil {
		return u, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind string
		var req, pt, ct, tt int64
		if err := rows.Scan(&kind, &req, &pt, &ct, &tt); err != nil {
			return u, err
		}
		switch kind {
		case "chat":
			u.ChatRequests, u.ChatPromptTokens, u.ChatComplTokens, u.ChatTotalTokens = req, pt, ct, tt
		case "embedding":
			u.EmbedRequests, u.EmbedTotalTokens = req, tt
		case "image":
			u.ImageRequests, u.ImageTotalTokens = req, tt
		}
		u.TotalTokens += tt
	}
	return u, rows.Err()
}

// UsageByDay returns the usage of the last n days, most recent first.
func (s *Store) UsageByDay(ctx context.Context, limit int) ([]UsageDay, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day,
		        SUM(CASE WHEN kind='chat' THEN total_tokens ELSE 0 END),
		        SUM(CASE WHEN kind='embedding' THEN total_tokens ELSE 0 END),
		        SUM(total_tokens),
		        SUM(requests)
		 FROM usage_daily GROUP BY day ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageDay
	for rows.Next() {
		var d UsageDay
		if err := rows.Scan(&d.Day, &d.ChatTokens, &d.EmbeddingTokens, &d.TotalTokens, &d.Requests); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UsageByModel returns the usage per model, largest first. Rows without a model
// name keep an empty Model field; the presentation layer localizes that case.
func (s *Store) UsageByModel(ctx context.Context) ([]UsageModel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, SUM(requests), SUM(total_tokens)
		 FROM usage_daily WHERE kind='chat'
		 GROUP BY model ORDER BY SUM(total_tokens) DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageModel
	for rows.Next() {
		var m UsageModel
		if err := rows.Scan(&m.Model, &m.Requests, &m.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- Helpers ----

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

const timeLayout = time.RFC3339Nano

func nowStr() string {
	return time.Now().UTC().Format(timeLayout)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// encodeEmbedding serializes a float32 vector as a little-endian BLOB.
func encodeEmbedding(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeEmbeddingInto deserializes a BLOB into dst, reusing its capacity when
// possible. It returns the (possibly reallocated) slice.
func decodeEmbeddingInto(dst []float32, b []byte) []float32 {
	n := len(b) / 4
	if cap(dst) < n {
		dst = make([]float32, n)
	}
	dst = dst[:n]
	for i := range n {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return dst
}
