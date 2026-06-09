package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Store kapselt den Zugriff auf die SQLite-Datenbank.
type Store struct {
	db   *sql.DB
	path string
}

// Open öffnet (oder erstellt) die SQLite-Datenbank am angegebenen Pfad.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	// SQLite verträgt bei Schreibzugriffen nur einen Writer.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close schließt die Datenbankverbindung.
func (s *Store) Close() error {
	return s.db.Close()
}

// DiskUsage liefert die aktuelle Größe der Datenbank in Bytes, inklusive der
// WAL- und SHM-Hilfsdateien (SQLite legt diese im WAL-Modus an).
func (s *Store) DiskUsage() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Ping prüft, ob die Datenbank erreichbar und schreibbar ist.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	// Schreibbarkeit verifizieren (Datenpfad gemountet & beschreibbar?).
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _healthcheck (id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	return nil
}

// Migrate legt das Schema an, falls es noch nicht existiert.
func (s *Store) Migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS chats (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	title      TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages(chat_id);
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
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Bestehende Datenbanken nachrüsten: chat_id-Spalte ergänzen, falls sie fehlt.
	if err := s.ensureDocumentChatColumn(ctx); err != nil {
		return err
	}
	return nil
}

// ensureDocumentChatColumn fügt documents.chat_id für ältere Schemata hinzu.
func (s *Store) ensureDocumentChatColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(documents)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	hasChatID := false
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
		if name == "chat_id" {
			hasChatID = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasChatID {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`ALTER TABLE documents ADD COLUMN chat_id INTEGER REFERENCES chats(id) ON DELETE CASCADE`)
	return err
}

// ---- Chats ----

// CreateChat legt einen neuen Chat an und liefert dessen ID.
func (s *Store) CreateChat(ctx context.Context, title string) (int64, error) {
	now := nowStr()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO chats (title, created_at, updated_at) VALUES (?, ?, ?)`,
		title, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListChats liefert alle Chats, neueste zuerst.
func (s *Store) ListChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, created_at, updated_at FROM chats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Title, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(created)
		c.UpdatedAt = parseTime(updated)
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// GetChat liefert einen einzelnen Chat.
func (s *Store) GetChat(ctx context.Context, id int64) (Chat, error) {
	var c Chat
	var created, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, created_at, updated_at FROM chats WHERE id = ?`, id).
		Scan(&c.ID, &c.Title, &created, &updated)
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

// UpdateChatTitle ändert den Titel eines Chats.
func (s *Store) UpdateChatTitle(ctx context.Context, id int64, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chats SET title = ?, updated_at = ? WHERE id = ?`, title, nowStr(), id)
	return err
}

// TouchChat aktualisiert den updated_at-Zeitstempel.
func (s *Store) TouchChat(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chats SET updated_at = ? WHERE id = ?`, nowStr(), id)
	return err
}

// DeleteChat entfernt einen Chat samt Nachrichten.
func (s *Store) DeleteChat(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chats WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Vacuum(ctx)
}

// ---- Messages ----

// AddMessage speichert eine Nachricht und liefert deren ID.
func (s *Store) AddMessage(ctx context.Context, chatID int64, role, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (chat_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		chatID, role, content, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListMessages liefert alle Nachrichten eines Chats in chronologischer Reihenfolge.
func (s *Store) ListMessages(ctx context.Context, chatID int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, chat_id, role, content, created_at FROM messages WHERE chat_id = ? ORDER BY id ASC`,
		chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

// ---- Documents & Chunks ----

// CreateDocument legt ein Dokument für einen Chat an und liefert dessen ID.
func (s *Store) CreateDocument(ctx context.Context, chatID int64, name, mime string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO documents (chat_id, name, mime, created_at) VALUES (?, ?, ?, ?)`,
		chatID, name, mime, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddChunk speichert einen Textabschnitt samt Embedding.
func (s *Store) AddChunk(ctx context.Context, documentID int64, ordinal int, text string, embedding []float32) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chunks (document_id, ordinal, text, embedding) VALUES (?, ?, ?, ?)`,
		documentID, ordinal, text, encodeEmbedding(embedding))
	return err
}

// ListDocumentsByChat liefert die Dokumente eines Chats mit Chunk-Anzahl.
func (s *Store) ListDocumentsByChat(ctx context.Context, chatID int64) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.chat_id, d.name, d.mime, d.created_at, COUNT(c.id)
		 FROM documents d LEFT JOIN chunks c ON c.document_id = d.id
		 WHERE d.chat_id = ?
		 GROUP BY d.id ORDER BY d.created_at DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

// DeleteDocument entfernt ein Dokument samt Chunks.
func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Vacuum(ctx)
}

// Vacuum schreibt die Datenbank ohne freie Seiten neu und gibt damit nach dem
// Löschen belegten Speicher an das Dateisystem zurück. VACUUM kann nicht in
// einer Transaktion laufen; bei sehr großen Datenbanken ist es kurzzeitig
// sperrend, für den persönlichen Maßstab hier aber unproblematisch.
//
// Im WAL-Modus landet das verkleinerte Ergebnis zunächst in der WAL-Datei; ein
// anschließender TRUNCATE-Checkpoint überträgt es in die Hauptdatei und schneidet
// die WAL ab, sodass die Dateigröße auf der Platte tatsächlich sinkt.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	return nil
}

// ChunksByChat liefert alle Chunks der Dokumente eines Chats samt Embeddings.
func (s *Store) ChunksByChat(ctx context.Context, chatID int64) ([]Chunk, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.document_id, c.ordinal, c.text, c.embedding
		 FROM chunks c JOIN documents d ON d.id = c.document_id
		 WHERE d.chat_id = ?`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		var blob []byte
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Ordinal, &c.Text, &blob); err != nil {
			return nil, err
		}
		c.Embedding = decodeEmbedding(blob)
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// CountChunksByChat liefert die Anzahl Chunks der Dokumente eines Chats.
func (s *Store) CountChunksByChat(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks c JOIN documents d ON d.id = c.document_id WHERE d.chat_id = ?`,
		chatID).Scan(&n)
	return n, err
}

// CountDocuments liefert die Gesamtanzahl gespeicherter Dokumente.
func (s *Store) CountDocuments(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents`).Scan(&n)
	return n, err
}

// ---- Helfer ----

// ErrNotFound wird zurückgegeben, wenn ein Datensatz nicht existiert.
var ErrNotFound = errors.New("nicht gefunden")

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

// encodeEmbedding serialisiert einen float32-Vektor als little-endian BLOB.
func encodeEmbedding(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeEmbedding deserialisiert einen BLOB zurück in einen float32-Vektor.
func decodeEmbedding(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
