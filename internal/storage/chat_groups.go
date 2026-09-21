package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxChatGroupTitle is the maximum group name length in Unicode characters.
const MaxChatGroupTitle = 80

// ErrInvalidChatGroup indicates invalid group metadata or identifiers.
var ErrInvalidChatGroup = errors.New("invalid chat group")

// ChatGroup organizes chats at a single level; groups cannot contain groups.
type ChatGroup struct {
	ID        int64
	Title     string
	Color     string
	Collapsed bool
	CreatedAt time.Time
}

// ChatGroupColors returns the finite set of safe CSS color tokens.
func ChatGroupColors() []string {
	return []string{"", "blue", "green", "amber", "rose", "violet"}
}

func validChatGroup(title, color string) bool {
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > MaxChatGroupTitle ||
		strings.ContainsFunc(title, unicode.IsControl) {
		return false
	}
	for _, allowed := range ChatGroupColors() {
		if color == allowed {
			return true
		}
	}
	return false
}

func (s *Store) migrateChatGroups(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS chat_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 80),
		color TEXT NOT NULL DEFAULT '' CHECK(color IN ('', 'blue', 'green', 'amber', 'rose', 'violet')),
		collapsed INTEGER NOT NULL DEFAULT 0 CHECK(collapsed IN (0, 1)),
		created_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, `PRAGMA table_info(chats)`, "group_id",
		`ALTER TABLE chats ADD COLUMN group_id INTEGER REFERENCES chat_groups(id) ON DELETE SET NULL`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, `PRAGMA table_info(chats)`, "keep_empty",
		`ALTER TABLE chats ADD COLUMN keep_empty INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_chats_group ON chats(group_id)`)
	return err
}

// ListChatGroups returns groups in stable creation order.
func (s *Store) ListChatGroups(ctx context.Context) ([]ChatGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, color, collapsed, created_at FROM chat_groups ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // Read-only rows have no pending writes.
	var groups []ChatGroup
	for rows.Next() {
		var group ChatGroup
		var created string
		if err := rows.Scan(&group.ID, &group.Title, &group.Color, &group.Collapsed, &created); err != nil {
			return nil, err
		}
		group.CreatedAt = parseTime(created)
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// GetChatGroup returns a group, including empty groups.
func (s *Store) GetChatGroup(ctx context.Context, id int64) (ChatGroup, error) {
	var group ChatGroup
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, title, color, collapsed, created_at FROM chat_groups WHERE id = ?`, id).
		Scan(&group.ID, &group.Title, &group.Color, &group.Collapsed, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return group, ErrNotFound
	}
	group.CreatedAt = parseTime(created)
	return group, err
}

// CreateChatGroup creates an empty group.
func (s *Store) CreateChatGroup(ctx context.Context, title, color string) (int64, error) {
	title = strings.TrimSpace(title)
	if !validChatGroup(title, color) {
		return 0, ErrInvalidChatGroup
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO chat_groups(title, color, created_at) VALUES (?, ?, ?)`, title, color, nowStr())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func changedChatGroup(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateChatGroup changes only a group's name and color.
func (s *Store) UpdateChatGroup(ctx context.Context, id int64, title, color string) error {
	title = strings.TrimSpace(title)
	if id <= 0 || !validChatGroup(title, color) {
		return ErrInvalidChatGroup
	}
	return changedChatGroup(s.db.ExecContext(ctx, `UPDATE chat_groups SET title = ?, color = ? WHERE id = ?`, title, color, id))
}

// SetChatGroupCollapsed persists the group disclosure state.
func (s *Store) SetChatGroupCollapsed(ctx context.Context, id int64, collapsed bool) error {
	if id <= 0 {
		return ErrInvalidChatGroup
	}
	return changedChatGroup(s.db.ExecContext(ctx, `UPDATE chat_groups SET collapsed = ? WHERE id = ?`, collapsed, id))
}

// DeleteChatGroup retains its chats by clearing their foreign keys.
func (s *Store) DeleteChatGroup(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidChatGroup
	}
	return changedChatGroup(s.db.ExecContext(ctx, `DELETE FROM chat_groups WHERE id = ?`, id))
}

// MoveChatToGroup retains explicitly organized empty chats and does not change
// conversation timestamps. Zero moves a chat back to the ungrouped section.
func (s *Store) MoveChatToGroup(ctx context.Context, chatID, groupID int64) error {
	if chatID <= 0 || groupID < 0 {
		return ErrInvalidChatGroup
	}
	result, err := s.db.ExecContext(ctx, `UPDATE chats SET group_id = NULLIF(?, 0), keep_empty = 1
		WHERE id = ? AND (? = 0 OR EXISTS (SELECT 1 FROM chat_groups WHERE id = ?))`,
		groupID, chatID, groupID, groupID)
	return changedChatGroup(result, err)
}
