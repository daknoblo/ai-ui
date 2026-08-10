package demo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"

	"github.com/daknoblo/ai-ui/internal/storage"
)

// IndexFile is the name of the file that describes a seeded demo instance.
const IndexFile = "demo-index.json"

// Index tells tooling (above all the screenshot script) which chat holds which
// demo section, so it can navigate without guessing database IDs.
type Index struct {
	Lang         string           `json:"lang"`
	Chats        map[string]int64 `json:"chats"`
	StreamPrompt string           `json:"stream_prompt"`
}

// WriteIndex stores the index next to the demo database.
func WriteIndex(dir string, idx Index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, IndexFile), append(data, '\n'), 0o600)
}

// StreamPrompt is the message that triggers a live answer from the stub
// backend. It is part of the demo so the question and the canned reply match.
func StreamPrompt(lang string) string {
	if lang == "de" {
		return "Nenne mir eine Drei-Schritte-Checkliste, um diese App sicher selbst zu hosten."
	}
	return "Give me a three-step checklist for hosting this app securely."
}

// Seed fills an empty database with the demo conversations, documents, images
// and token statistics. A database that already holds chats is left untouched;
// the index is then rebuilt from the stored titles so restarting the demo keeps
// working.
func Seed(ctx context.Context, store *storage.Store, dbPath, lang string) (Index, error) {
	convs, err := conversations(lang)
	if err != nil {
		return Index{}, err
	}
	idx := Index{Lang: lang, Chats: make(map[string]int64, len(convs)), StreamPrompt: StreamPrompt(lang)}

	existing, err := store.ListChats(ctx)
	if err != nil {
		return idx, err
	}
	if len(existing) > 0 {
		byTitle := make(map[string]int64, len(existing))
		for _, c := range existing {
			byTitle[c.Title] = c.ID
		}
		for _, conv := range convs {
			if id, ok := byTitle[conv.Title]; ok {
				idx.Chats[conv.Key] = id
			}
		}
		return idx, nil
	}

	ids := make([]int64, 0, len(convs))
	for _, conv := range convs {
		id, err := seedConversation(ctx, store, conv)
		if err != nil {
			return idx, fmt.Errorf("seed %q: %w", conv.Key, err)
		}
		idx.Chats[conv.Key] = id
		ids = append(ids, id)
	}
	if err := seedTimeline(dbPath, ids); err != nil {
		return idx, err
	}
	return idx, nil
}

// seedConversation writes one demo chat including its attachments.
func seedConversation(ctx context.Context, store *storage.Store, conv conversation) (int64, error) {
	chatID, err := store.CreateChat(ctx, conv.Title, conv.Model)
	if err != nil {
		return 0, err
	}
	if conv.Mode == storage.ChatModeImage {
		if err := store.UpdateChatMode(ctx, chatID, storage.ChatModeImage); err != nil {
			return 0, err
		}
	}

	for _, doc := range conv.Docs {
		docID, err := store.CreateDocument(ctx, chatID, doc.Name, doc.MIME)
		if err != nil {
			return 0, err
		}
		texts := make([]string, doc.Chunks)
		vectors := make([][]float32, doc.Chunks)
		for i := range texts {
			texts[i] = chunkText(doc, i)
			vectors[i] = Embedding(texts[i])
		}
		if err := store.AddChunks(ctx, docID, texts, vectors); err != nil {
			return 0, err
		}
	}

	if conv.Upload != "" {
		data, mime, err := encodeImage(renderIllustration(768, 512, accentFor(conv.Upload)), "png")
		if err != nil {
			return 0, err
		}
		if _, err := store.AddImage(ctx, chatID, storage.ImageUpload, conv.Upload, "", mime, data); err != nil {
			return 0, err
		}
	}

	imageID := int64(0)
	if conv.Image != "" {
		data, mime, err := encodeImage(renderIllustration(1536, 1024, accentFor(conv.Image)), "png")
		if err != nil {
			return 0, err
		}
		imageID, err = store.AddImage(ctx, chatID, storage.ImageGenerated, "", conv.Image, mime, data)
		if err != nil {
			return 0, err
		}
	}

	for _, msg := range conv.Messages {
		body := strings.ReplaceAll(msg.Content, imagePlaceholder, strconv.FormatInt(imageID, 10))
		if _, err := store.AddMessage(ctx, chatID, msg.Role, body); err != nil {
			return 0, err
		}
	}
	return chatID, nil
}

// chunkText builds the stored text of a demo chunk. It is never shown in the
// UI; it only has to read like a document section for the retrieval step.
func chunkText(doc attachment, ordinal int) string {
	return fmt.Sprintf("%s — section %d\n\nThis passage of %s is demo content. "+
		"It exists so the retrieval step has something to score and the document "+
		"chip shows a realistic chunk count.", doc.Name, ordinal+1, doc.Name)
}

// embeddingDim is the vector length of the demo embeddings.
const embeddingDim = 256

// Embedding turns a text into a deterministic vector. It is a hashed bag of
// words rather than a semantic embedding: it only has to be stable and to give
// related texts a measurable similarity, which is enough for the demo.
func Embedding(text string) []float32 {
	vec := make([]float32, embeddingDim)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, word := range words {
		h := fnv.New32a()
		_, _ = h.Write([]byte(word))
		vec[h.Sum32()%embeddingDim]++
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// usageDays is the number of days the demo statistics cover.
const usageDays = 14

// seedTimeline spreads the demo chats over the past hours and fills the token
// statistics. Both need timestamps that the storage API deliberately does not
// expose, so they are written directly.
func seedTimeline(dbPath string, ids []int64) error {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	// The sidebar orders by updated_at, so the first conversation stays on top.
	for i, id := range ids {
		ts := now.Add(-time.Duration(i*97+11) * time.Minute).Format(time.RFC3339Nano)
		if _, err := db.Exec(`UPDATE chats SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE messages SET created_at = ? WHERE chat_id = ?`, ts, id); err != nil {
			return err
		}
	}

	chatModels := []string{"gpt-5.5", "claude-opus-4-7", "o4-mini", ""}
	for day := 0; day < usageDays; day++ {
		date := now.AddDate(0, 0, -day).Format("2006-01-02")
		for i, model := range chatModels {
			requests := 6 + int(pseudo(day, i)*24)
			prompt := requests * (420 + int(pseudo(day, i+10)*900))
			completion := requests * (180 + int(pseudo(day, i+20)*400))
			if err := insertUsage(db, date, "chat", model, requests, prompt, completion); err != nil {
				return err
			}
		}
		embedRequests := 3 + int(pseudo(day, 31)*9)
		if err := insertUsage(db, date, "embedding", "text-embedding-3-large",
			embedRequests, embedRequests*1150, 0); err != nil {
			return err
		}
		if day%3 == 0 {
			images := 1 + int(pseudo(day, 41)*3)
			if err := insertUsage(db, date, "image", "gpt-image-2", images, images*90, images*1580); err != nil {
				return err
			}
		}
	}
	return nil
}

// insertUsage books one aggregated statistics row.
func insertUsage(db *sql.DB, day, kind, model string, requests, prompt, completion int) error {
	_, err := db.Exec(
		`INSERT INTO usage_daily (day, kind, model, requests, prompt_tokens, completion_tokens, total_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		day, kind, model, requests, prompt, completion, prompt+completion)
	return err
}

// pseudo returns a deterministic value in [0,1) for a pair of coordinates. It
// replaces math/rand so the demo data only changes when the code changes.
func pseudo(a, b int) float64 {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%d:%d", a, b)
	return float64(h.Sum32()%10_000) / 10_000
}
