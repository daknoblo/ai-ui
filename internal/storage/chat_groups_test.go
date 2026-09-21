package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatGroupsMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }() // Test database cleanup.
	ctx := t.Context()
	_, err = store.db.ExecContext(ctx, `CREATE TABLE chats (
		id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
		INSERT INTO chats(title, created_at, updated_at) VALUES ('legacy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	group, err := store.CreateChatGroup(ctx, " Research ", "blue")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MoveChatToGroup(ctx, 1, group); err != nil {
		t.Fatal(err)
	}
	if err := store.SetChatGroupCollapsed(ctx, group, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetChatGroup(ctx, group)
	if err != nil || got.Title != "Research" || got.Color != "blue" || !got.Collapsed {
		t.Fatalf("persistent group = %+v, %v", got, err)
	}
	chat, err := store.GetChat(ctx, 1)
	if err != nil || chat.Title != "legacy" || chat.GroupID != group {
		t.Fatalf("legacy chat = %+v, %v", chat, err)
	}
}

func TestChatGroupsMoveRetainAndDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	first, err := store.CreateChatGroup(ctx, "One", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateChatGroup(ctx, "Two", "green")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.CreateChat(ctx, "", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.GetChat(ctx, chat)
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range []int64{first, second, 0, first} {
		if err := store.MoveChatToGroup(ctx, chat, destination); err != nil {
			t.Fatal(err)
		}
		if count, err := store.DeleteEmptyChats(ctx, 0); err != nil || count != 0 {
			t.Fatalf("organized empty chat removed: %d, %v", count, err)
		}
		got, err := store.GetChat(ctx, chat)
		if err != nil || got.GroupID != destination || !got.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("moved chat = %+v, %v", got, err)
		}
	}
	if err := store.DeleteChatGroup(ctx, first); err != nil {
		t.Fatal(err)
	}
	if count, err := store.DeleteEmptyChats(ctx, 0); err != nil || count != 0 {
		t.Fatalf("group removal lost chat: %d, %v", count, err)
	}
	got, err := store.GetChat(ctx, chat)
	if err != nil || got.GroupID != 0 {
		t.Fatalf("ungrouped chat = %+v, %v", got, err)
	}
	if err := store.UpdateChatGroup(ctx, second, "Renamed", "violet"); err != nil {
		t.Fatal(err)
	}
	groups, err := store.ListChatGroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Title != "Renamed" {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	if err := store.MoveChatToGroup(ctx, chat, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessage(ctx, chat, "user", "retained until deletion"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDocument(ctx, chat, "doc.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if messages, err := store.ListMessages(ctx, chat); err != nil || len(messages) != 0 {
		t.Fatalf("messages = %+v, %v", messages, err)
	}
	if docs, err := store.CountDocuments(ctx); err != nil || docs != 0 {
		t.Fatalf("documents = %d, %v", docs, err)
	}
	if _, err := store.GetChatGroup(ctx, second); err != nil {
		t.Fatalf("deleting chat removed its group: %v", err)
	}
}

func TestRemovingGroupPreservesConversationAssets(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	group, err := store.CreateChatGroup(ctx, "Media", "blue")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.CreateChat(ctx, "Retained conversation", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MoveChatToGroup(ctx, chat, group); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessage(ctx, chat, "assistant", "Retained answer"); err != nil {
		t.Fatal(err)
	}
	document, err := store.CreateDocument(ctx, chat, "context.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddChunks(ctx, document, []string{"Retained context"}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	image, err := store.AddImage(ctx, chat, "generated", "", "Retained prompt", "image/png", []byte("retained-image"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteChatGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if messages, err := store.ListMessages(ctx, chat); err != nil || len(messages) != 1 || messages[0].Content != "Retained answer" {
		t.Fatalf("group removal changed messages: %+v, %v", messages, err)
	}
	if count, err := store.CountChunksByChat(ctx, chat); err != nil || count != 1 {
		t.Fatalf("group removal changed document chunks: %d, %v", count, err)
	}
	if img, err := store.GetImage(ctx, image); err != nil || img.ChatID != chat || string(img.Data) != "retained-image" {
		t.Fatalf("group removal changed its image: %+v, %v", img, err)
	}
}

func TestChatGroupsValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	for _, input := range []struct{ title, color string }{
		{"", ""}, {" \n ", ""}, {strings.Repeat("a", 81), ""},
		{"line\nbreak", ""}, {"ok", "red"}, {"ok", `"><script>`},
	} {
		if _, err := store.CreateChatGroup(ctx, input.title, input.color); !errors.Is(err, ErrInvalidChatGroup) {
			t.Errorf("input %+v = %v", input, err)
		}
	}
	for _, color := range ChatGroupColors() {
		if _, err := store.CreateChatGroup(ctx, strings.Repeat("ä", 80), color); err != nil {
			t.Fatal(err)
		}
	}
	for name, err := range map[string]error{
		"update":   store.UpdateChatGroup(ctx, 9999, "Missing", ""),
		"collapse": store.SetChatGroupCollapsed(ctx, 9999, true),
		"delete":   store.DeleteChatGroup(ctx, 9999),
		"chat":     store.MoveChatToGroup(ctx, 9999, 0),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v", name, err)
		}
	}
	chat, err := store.CreateChat(ctx, "chat", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MoveChatToGroup(ctx, chat, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := store.MoveChatToGroup(ctx, chat, -1); !errors.Is(err, ErrInvalidChatGroup) {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE chats SET group_id = 9999 WHERE id = ?`, chat); err == nil {
		t.Fatal("foreign key accepted a missing group")
	}
	if count, err := store.DeleteEmptyChats(ctx, 0); err != nil || count != 1 {
		t.Fatalf("ordinary empty chat not cleaned: %d, %v", count, err)
	}
}
