package storage

import "time"

// Chat ist eine Konversation in der Seitenleiste.
type Chat struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message ist eine einzelne Nachricht innerhalb eines Chats.
type Message struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Role      string    `json:"role"` // "user" oder "assistant"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Document beschreibt ein hochgeladenes Dokument.
type Document struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Name      string    `json:"name"`
	MIME      string    `json:"mime"`
	Chunks    int       `json:"chunks"`
	CreatedAt time.Time `json:"created_at"`
}

// Chunk ist ein Textabschnitt eines Dokuments samt Embedding.
type Chunk struct {
	ID         int64
	DocumentID int64
	Ordinal    int
	Text       string
	Embedding  []float32
}
