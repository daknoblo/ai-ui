package storage

import "time"

// Chat is a conversation shown in the sidebar.
type Chat struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Model     string    `json:"model"` // pinned model of this chat (empty = router decides)
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message is a single message within a chat.
type Message struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Role      string    `json:"role"` // "user" or "assistant"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Document describes an uploaded document.
type Document struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Name      string    `json:"name"`
	MIME      string    `json:"mime"`
	Chunks    int       `json:"chunks"`
	CreatedAt time.Time `json:"created_at"`
}

// Image is an image generated in a chat.
type Image struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Prompt    string    `json:"prompt"`
	MIME      string    `json:"mime"`
	Data      []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// ChunkVector is a chunk reduced to the fields needed for similarity scoring.
type ChunkVector struct {
	ID         int64
	DocumentID int64
	Embedding  []float32
}
