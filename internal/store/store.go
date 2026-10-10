// Package store persists users, settings, conversations, messages, and
// projects in SQLite per contract §2, §8, and §9. It uses modernc.org/sqlite
// (pure Go, driver name "sqlite") so the binary builds with CGO_ENABLED=0.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist (or is not owned by the
// requesting user).
var ErrNotFound = errors.New("not found")

// schema is the DDL from contract §2, §8.1, and §9.1.
const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL DEFAULT 'active',
  api_key_enc   TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS projects (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  instructions  TEXT NOT NULL DEFAULT '',
  default_model TEXT,
  sort_order    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS conversations (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL,
  effort     TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id                TEXT PRIMARY KEY,
  conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role              TEXT NOT NULL,
  content           TEXT NOT NULL,
  reasoning         TEXT,
  model             TEXT,
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  cached_tokens     INTEGER,
  cost_micros       INTEGER,
  created_at        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS attachments (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  mime       TEXT,
  size       INTEGER NOT NULL,
  text       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_convo ON messages(conversation_id, created_at);
CREATE INDEX IF NOT EXISTS idx_convos_updated ON conversations(updated_at DESC);
CREATE TABLE IF NOT EXISTS groups (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  share_key  INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS group_members (
  group_id   TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL DEFAULT 'member',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_group_members_user ON group_members(user_id);
CREATE INDEX IF NOT EXISTS idx_groups_owner ON groups(owner_id);
CREATE INDEX IF NOT EXISTS idx_attachments_user ON attachments(user_id);
CREATE TABLE IF NOT EXISTS geo_cache (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
`

// User mirrors a row of the users table (§9.1).
type User struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	Email        string  `json:"email"`
	PasswordHash string  `json:"-"`
	IsAdmin      bool    `json:"is_admin"`
	Status       string  `json:"status"` // active | pending | disabled
	APIKeyEnc    *string `json:"-"`      // encrypted per-user OpenCode Go key (§2.1)
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
}

// Conversation mirrors a row of the conversations table.
type Conversation struct {
	ID        string  `json:"id"`
	UserID    *string `json:"-"`
	Title     string  `json:"title"`
	Model     string  `json:"model"`
	Effort    *string `json:"effort"`
	ProjectID *string `json:"project_id"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

// Message mirrors a row of the messages table.
type Message struct {
	ID               string         `json:"id"`
	ConversationID   string         `json:"conversation_id"`
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	Reasoning        *string        `json:"reasoning"`
	Model            *string        `json:"model"`
	PromptTokens     *int64         `json:"prompt_tokens"`
	CompletionTokens *int64         `json:"completion_tokens"`
	CachedTokens     *int64         `json:"cached_tokens"`
	CostMicros       *int64         `json:"cost_micros"`
	ReasoningTokens  *int64         `json:"reasoning_tokens"`
	DurationMs       *int64         `json:"duration_ms"`
	Attachments      []AttachedFile `json:"attachments,omitempty"`
	CreatedAt        int64          `json:"created_at"`
}

// AttachedFile is the display metadata of a file attached to a user message.
// It is persisted as a JSON array on messages.attachments (2.3) so the UI can
// show "N file(s) attached" after a reload; the extracted text itself lives in
// the attachments table and is only folded into the request at send time.
type AttachedFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size"`
}

// Attachment mirrors a row of the attachments table (2.3). Text is the
// extracted plain text folded into the model request.
type Attachment struct {
	ID        string `json:"id"`
	UserID    string `json:"-"`
	Name      string `json:"name"`
	Mime      string `json:"mime,omitempty"`
	Size      int64  `json:"size"`
	Text      string `json:"-"`
	CreatedAt int64  `json:"created_at"`
}

// Chars reports the number of characters in the extracted text (rune count).
func (a Attachment) Chars() int { return len([]rune(a.Text)) }

// Project mirrors a row of the projects table (§8.2).
type Project struct {
	ID                string  `json:"id"`
	UserID            *string `json:"-"`
	Name              string  `json:"name"`
	Instructions      string  `json:"instructions"`
	DefaultModel      *string `json:"default_model"`
	SortOrder         int64   `json:"sort_order"`
	CreatedAt         int64   `json:"created_at"`
	UpdatedAt         int64   `json:"updated_at"`
	ConversationCount int64   `json:"conversation_count"`
}

// Group mirrors a row of the groups table (§2.2). IsOwner and Members are
// filled by the read helpers relative to the requesting user.
type Group struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	OwnerID   string        `json:"owner_id"`
	ShareKey  bool          `json:"share_key"`
	CreatedAt int64         `json:"created_at"`
	UpdatedAt int64         `json:"updated_at"`
	IsOwner   bool          `json:"is_owner"`
	Members   []GroupMember `json:"members"`
}

// GroupMember is a group_members row joined with the user's display fields.
type GroupMember struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open creates parent directories, opens the database in WAL mode with foreign
// keys enabled, applies the schema, and migrates older databases.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Single-user app: one connection serializes access and avoids
	// SQLITE_BUSY on concurrent writes.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return s, nil
}

// migrate adds columns introduced after earlier releases without dropping data.
func (s *Store) migrate() error {
	steps := []struct {
		table, column, ddl string
	}{
		{"conversations", "project_id", `ALTER TABLE conversations ADD COLUMN project_id TEXT REFERENCES projects(id) ON DELETE SET NULL`},
		{"messages", "reasoning_tokens", `ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER`},
		{"messages", "duration_ms", `ALTER TABLE messages ADD COLUMN duration_ms INTEGER`},
		{"messages", "attachments", `ALTER TABLE messages ADD COLUMN attachments TEXT`},
		{"conversations", "user_id", `ALTER TABLE conversations ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`},
		{"projects", "user_id", `ALTER TABLE projects ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`},
		{"users", "status", `ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`},
		{"users", "api_key_enc", `ALTER TABLE users ADD COLUMN api_key_enc TEXT`},
	}
	cache := map[string]map[string]bool{}
	for _, st := range steps {
		cols, ok := cache[st.table]
		if !ok {
			var err error
			cols, err = s.columns(st.table)
			if err != nil {
				return err
			}
			cache[st.table] = cols
		}
		if cols[st.column] {
			continue
		}
		if _, err := s.db.Exec(st.ddl); err != nil {
			return err
		}
		cols[st.column] = true
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_convos_project ON conversations(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_convos_user ON conversations(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_projects_user ON projects(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_geo_cache_expires ON geo_cache(expires_at)`,
	} {
		if _, err := s.db.Exec(idx); err != nil {
			return err
		}
	}
	return nil
}

// columns returns the set of column names of a table.
func (s *Store) columns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

type scanner interface{ Scan(dest ...any) error }

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func optString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func optInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// ---------- users ----------

const userCols = "id, username, email, password_hash, is_admin, status, api_key_enc, created_at, updated_at"

func scanUser(sc scanner) (User, error) {
	var u User
	var admin int
	var keyEnc sql.NullString
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &admin, &u.Status, &keyEnc, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, err
	}
	u.IsAdmin = admin != 0
	u.APIKeyEnc = optString(keyEnc)
	return u, nil
}

// InsertUser inserts a new user row; duplicate username/email surface as a
// driver error (the caller maps it to 409).
func (s *Store) InsertUser(ctx context.Context, u *User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.Email, u.PasswordHash, boolToInt(u.IsAdmin), u.Status, nullString(u.APIKeyEnc), u.CreatedAt, u.UpdatedAt)
	return err
}

// GetUserByID loads a user by id, or ErrNotFound.
func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// GetUserByIdentifier loads a user by username or email (case-insensitive).
func (s *Store) GetUserByIdentifier(ctx context.Context, ident string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users
		 WHERE username = ? COLLATE NOCASE OR email = ? COLLATE NOCASE LIMIT 1`, ident, ident))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// ListUsers returns all users ordered by creation time.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userCols+` FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CountUsers returns the number of users (0 → first-run setup).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&n)
	return n, err
}

// UpdateUserPassword sets a user's password hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id, hash string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, now, id)
	return err
}

// SetUserAdmin sets a user's admin flag.
func (s *Store) SetUserAdmin(ctx context.Context, id string, admin bool, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET is_admin = ?, updated_at = ? WHERE id = ?`, boolToInt(admin), now, id)
	return err
}

// SetUserStatus sets a user's status (active | pending | disabled).
func (s *Store) SetUserStatus(ctx context.Context, id, status string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET status = ?, updated_at = ? WHERE id = ?`, status, now, id)
	return err
}

// SetUserAPIKey stores (or clears, when nil) a user's encrypted API key.
func (s *Store) SetUserAPIKey(ctx context.Context, id string, enc *string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET api_key_enc = ?, updated_at = ? WHERE id = ?`, nullString(enc), now, id)
	return err
}

// CountAdmins returns the number of active admin accounts (lockout guard).
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM users WHERE is_admin = 1 AND status = 'active'`).Scan(&n)
	return n, err
}

// CountPendingUsers returns the number of accounts awaiting approval.
func (s *Store) CountPendingUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// DeleteUser deletes a user; their conversations and projects cascade.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// ClaimLegacyData assigns all rows with no owner to userID. Called once when
// the first admin is created so pre-existing chats survive the migration.
func (s *Store) ClaimLegacyData(ctx context.Context, userID string) error {
	for _, q := range []string{
		`UPDATE conversations SET user_id = ? WHERE user_id IS NULL`,
		`UPDATE projects SET user_id = ? WHERE user_id IS NULL`,
	} {
		if _, err := s.db.ExecContext(ctx, q, userID); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- settings ----------

// GetSetting returns a setting value and whether it exists.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting upserts a setting value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// EnsureSetting returns the value of key, creating it with def when missing.
func (s *Store) EnsureSetting(ctx context.Context, key, def string) (string, error) {
	if v, ok, err := s.GetSetting(ctx, key); err != nil {
		return "", err
	} else if ok {
		return v, nil
	}
	if err := s.SetSetting(ctx, key, def); err != nil {
		return "", err
	}
	return def, nil
}

// ---------- conversations ----------

const conversationCols = "id, title, model, effort, project_id, created_at, updated_at"

func scanConversation(sc scanner) (Conversation, error) {
	var c Conversation
	var effort, project sql.NullString
	if err := sc.Scan(&c.ID, &c.Title, &c.Model, &effort, &project, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Conversation{}, err
	}
	c.Effort = optString(effort)
	c.ProjectID = optString(project)
	return c, nil
}

// InsertConversation inserts a new conversation row for its owner.
func (s *Store) InsertConversation(ctx context.Context, c *Conversation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, user_id, title, model, effort, project_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, nullString(c.UserID), c.Title, c.Model, nullString(c.Effort), nullString(c.ProjectID), c.CreatedAt, c.UpdatedAt)
	return err
}

// GetConversation loads one conversation owned by userID, else ErrNotFound.
func (s *Store) GetConversation(ctx context.Context, id, userID string) (Conversation, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationCols+` FROM conversations WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	return c, err
}

// ListConversations returns the user's conversations, updated_at DESC.
func (s *Store) ListConversations(ctx context.Context, userID string) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationCols+` FROM conversations WHERE user_id = ? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	convs := make([]Conversation, 0)
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		convs = append(convs, c)
	}
	return convs, rows.Err()
}

// UpdateConversation updates a conversation's mutable fields (owner-guarded).
func (s *Store) UpdateConversation(ctx context.Context, c *Conversation) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = ?, model = ?, effort = ?, project_id = ?, updated_at = ?
		 WHERE id = ? AND user_id = ?`,
		c.Title, c.Model, nullString(c.Effort), nullString(c.ProjectID), c.UpdatedAt, c.ID, nullString(c.UserID))
	return err
}

// SetTitle updates only the conversation title (used by auto-title).
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	return err
}

// TouchConversation bumps updated_at (last activity).
func (s *Store) TouchConversation(ctx context.Context, id string, updatedAt int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, updatedAt, id)
	return err
}

// DeleteConversation deletes an owner's conversation; messages cascade. It
// returns the number of rows removed (0 when not owned by userID).
func (s *Store) DeleteConversation(ctx context.Context, id, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOlderThan deletes conversations updated before cutoff (unix ms),
// across all users, for the retention janitor.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- messages ----------

const messageCols = "id, conversation_id, role, content, reasoning, model, " +
	"prompt_tokens, completion_tokens, cached_tokens, cost_micros, " +
	"reasoning_tokens, duration_ms, attachments, created_at"

func scanMessage(sc scanner) (Message, error) {
	var m Message
	var reasoning, model, attachments sql.NullString
	var promptTokens, completionTokens, cachedTokens, costMicros sql.NullInt64
	var reasoningTokens, durationMs sql.NullInt64
	err := sc.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content,
		&reasoning, &model, &promptTokens, &completionTokens, &cachedTokens,
		&costMicros, &reasoningTokens, &durationMs, &attachments, &m.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	m.Reasoning = optString(reasoning)
	m.Model = optString(model)
	m.PromptTokens = optInt(promptTokens)
	m.CompletionTokens = optInt(completionTokens)
	m.CachedTokens = optInt(cachedTokens)
	m.CostMicros = optInt(costMicros)
	m.ReasoningTokens = optInt(reasoningTokens)
	m.DurationMs = optInt(durationMs)
	if attachments.Valid && attachments.String != "" {
		// A malformed value must not fail the whole history load; drop it.
		_ = json.Unmarshal([]byte(attachments.String), &m.Attachments)
	}
	return m, nil
}

// InsertMessage inserts a new message row.
func (s *Store) InsertMessage(ctx context.Context, m *Message) error {
	var attachments any
	if len(m.Attachments) > 0 {
		b, err := json.Marshal(m.Attachments)
		if err != nil {
			return err
		}
		attachments = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (`+messageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, m.Role, m.Content, nullString(m.Reasoning),
		nullString(m.Model), nullInt(m.PromptTokens), nullInt(m.CompletionTokens),
		nullInt(m.CachedTokens), nullInt(m.CostMicros), nullInt(m.ReasoningTokens),
		nullInt(m.DurationMs), attachments, m.CreatedAt)
	return err
}

// ListMessages returns all messages of a conversation ascending by created_at.
func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE conversation_id = ? ORDER BY created_at ASC, rowid ASC`,
		conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// DeleteAssistantAfterLastUser removes assistant messages that follow the last
// user message, so a regenerate can replace the final reply.
func (s *Store) DeleteAssistantAfterLastUser(ctx context.Context, conversationID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM messages
		 WHERE conversation_id = ? AND role = 'assistant'
		   AND rowid > (SELECT COALESCE(MAX(rowid), 0) FROM messages
		                WHERE conversation_id = ? AND role = 'user')`,
		conversationID, conversationID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// HasUserMessage reports whether a conversation has at least one user message.
func (s *Store) HasUserMessage(ctx context.Context, conversationID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM messages WHERE conversation_id = ? AND role = 'user'`,
		conversationID).Scan(&n)
	return n > 0, err
}

// ---------- projects ----------

const projectCols = "id, name, instructions, default_model, sort_order, created_at, updated_at"

func scanProject(sc scanner) (Project, error) {
	var p Project
	var model sql.NullString
	if err := sc.Scan(&p.ID, &p.Name, &p.Instructions, &model, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Project{}, err
	}
	p.DefaultModel = optString(model)
	return p, nil
}

// InsertProject inserts a new project row for its owner.
func (s *Store) InsertProject(ctx context.Context, p *Project) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, user_id, name, instructions, default_model, sort_order, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, nullString(p.UserID), p.Name, p.Instructions, nullString(p.DefaultModel), p.SortOrder, p.CreatedAt, p.UpdatedAt)
	return err
}

// GetProject loads one project owned by userID, else ErrNotFound.
func (s *Store) GetProject(ctx context.Context, id, userID string) (Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectCols+` FROM projects WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	counts, err := s.projectCounts(ctx, userID)
	if err != nil {
		return Project{}, err
	}
	p.ConversationCount = counts[p.ID]
	return p, nil
}

// ListProjects returns the user's projects with conversation counts.
func (s *Store) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectCols+` FROM projects WHERE user_id = ? ORDER BY sort_order ASC, created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := make([]Project, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts, err := s.projectCounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		projects[i].ConversationCount = counts[projects[i].ID]
	}
	return projects, nil
}

// UpdateProject updates a project's mutable fields (owner-guarded).
func (s *Store) UpdateProject(ctx context.Context, p *Project) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, instructions = ?, default_model = ?, sort_order = ?, updated_at = ?
		 WHERE id = ? AND user_id = ?`,
		p.Name, p.Instructions, nullString(p.DefaultModel), p.SortOrder, p.UpdatedAt, p.ID, nullString(p.UserID))
	return err
}

// DeleteProject deletes a project; its conversations are moved to Unfiled.
func (s *Store) DeleteProject(ctx context.Context, id, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET project_id = NULL WHERE project_id = ? AND user_id = ?`, id, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM projects WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// projectCounts returns the number of conversations per project for a user.
func (s *Store) projectCounts(ctx context.Context, userID string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, COUNT(1) FROM conversations
		 WHERE user_id = ? AND project_id IS NOT NULL GROUP BY project_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		counts[id] = n
	}
	return counts, rows.Err()
}

// ---------- groups (§2.2) ----------

const groupCols = "id, name, owner_id, share_key, created_at, updated_at"

func scanGroup(sc scanner) (Group, error) {
	var g Group
	var share int
	if err := sc.Scan(&g.ID, &g.Name, &g.OwnerID, &share, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return Group{}, err
	}
	g.ShareKey = share != 0
	return g, nil
}

// InsertGroup inserts a new group; OwnerID is the creator.
func (s *Store) InsertGroup(ctx context.Context, g *Group) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO groups (`+groupCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		g.ID, g.Name, g.OwnerID, boolToInt(g.ShareKey), g.CreatedAt, g.UpdatedAt)
	return err
}

// GetGroup loads one group by id (with members), else ErrNotFound.
func (s *Store) GetGroup(ctx context.Context, id string) (Group, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx,
		`SELECT `+groupCols+` FROM groups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, err
	}
	members, err := s.ListGroupMembers(ctx, id)
	if err != nil {
		return Group{}, err
	}
	g.Members = members
	return g, nil
}

// ListGroupsForUser returns the groups the user owns or belongs to, oldest
// first. IsOwner and Members are set relative to userID.
func (s *Store) ListGroupsForUser(ctx context.Context, userID string) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+groupCols+` FROM groups
		 WHERE owner_id = ?
		    OR id IN (SELECT group_id FROM group_members WHERE user_id = ?)
		 ORDER BY created_at ASC`, userID, userID)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0)
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// Close before the per-group member queries: the pool is a single
	// connection, so an open result set would deadlock the next query.
	rows.Close()
	for i := range groups {
		members, err := s.ListGroupMembers(ctx, groups[i].ID)
		if err != nil {
			return nil, err
		}
		groups[i].Members = members
		groups[i].IsOwner = groups[i].OwnerID == userID
	}
	return groups, nil
}

// ListKeyGroupsForUser returns the groups whose owner may share their key with
// userID (membership only, sharing on), oldest first. The caller resolves each
// owner's stored key in order (§2.2).
func (s *Store) ListKeyGroupsForUser(ctx context.Context, userID string) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.id, g.name, g.owner_id, g.share_key, g.created_at, g.updated_at
		   FROM groups g JOIN group_members m ON m.group_id = g.id
		  WHERE m.user_id = ? AND g.share_key = 1
		  ORDER BY g.created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// UpdateGroup updates a group's name and sharing flag (owner-guarded). It
// returns ErrNotFound when the group is missing or not owned by OwnerID.
func (s *Store) UpdateGroup(ctx context.Context, g *Group) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE groups SET name = ?, share_key = ?, updated_at = ?
		  WHERE id = ? AND owner_id = ?`,
		g.Name, boolToInt(g.ShareKey), g.UpdatedAt, g.ID, g.OwnerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteGroup deletes an owner's group; memberships cascade. It returns the
// number of rows removed.
func (s *Store) DeleteGroup(ctx context.Context, id, ownerID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM groups WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AddGroupMember inserts a membership row (duplicates surface as a driver
// error, mapped to 409 by the caller).
func (s *Store) AddGroupMember(ctx context.Context, groupID, userID, role string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO group_members (group_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		groupID, userID, role, now)
	return err
}

// RemoveGroupMember deletes a membership row; returns rows removed.
func (s *Store) RemoveGroupMember(ctx context.Context, groupID, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListGroupMembers returns a group's members joined with user display fields.
func (s *Store) ListGroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.username, u.email, m.role
		   FROM group_members m JOIN users u ON u.id = m.user_id
		  WHERE m.group_id = ?
		  ORDER BY m.created_at ASC, u.username ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := make([]GroupMember, 0)
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.UserID, &m.Username, &m.Email, &m.Role); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// IsGroupMember reports whether userID belongs to groupID.
func (s *Store) IsGroupMember(ctx context.Context, groupID, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM group_members WHERE group_id = ? AND user_id = ?`,
		groupID, userID).Scan(&n)
	return n > 0, err
}

// ---------- attachments (2.3) ----------

const attachmentCols = "id, user_id, name, mime, size, text, created_at"

func scanAttachment(sc scanner) (Attachment, error) {
	var a Attachment
	var mime sql.NullString
	if err := sc.Scan(&a.ID, &a.UserID, &a.Name, &mime, &a.Size, &a.Text, &a.CreatedAt); err != nil {
		return Attachment{}, err
	}
	a.Mime = ""
	if mime.Valid {
		a.Mime = mime.String
	}
	return a, nil
}

// InsertAttachment stores an extracted file for its owner.
func (s *Store) InsertAttachment(ctx context.Context, a *Attachment) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO attachments (`+attachmentCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.Name, nullString(&a.Mime), a.Size, a.Text, a.CreatedAt)
	return err
}

// GetAttachment loads one attachment owned by userID, else ErrNotFound. The
// owner guard makes cross-user access indistinguishable from a missing row.
func (s *Store) GetAttachment(ctx context.Context, id, userID string) (Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentCols+` FROM attachments WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return a, err
}

// DeleteAttachment removes an owner's attachment. It returns the number of
// rows removed (0 when not owned by userID).
func (s *Store) DeleteAttachment(ctx context.Context, id, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAttachmentsOlderThan purges attachments created before cutoff (unix
// ms) across all users, for the retention janitor.
func (s *Store) DeleteAttachmentsOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- geo cache (3.4) ----------

// GetGeoCache returns a cached maps provider result when it exists and has not
// expired (expires_at is unix milliseconds). It satisfies tools.GeoCache.
func (s *Store) GetGeoCache(ctx context.Context, key string) (string, bool, error) {
	var (
		value     string
		expiresAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at FROM geo_cache WHERE key = ?`, key).Scan(&value, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if expiresAt <= time.Now().UnixMilli() {
		return "", false, nil
	}
	return value, true, nil
}

// PutGeoCache upserts a cached maps provider result until expiresAt (unix ms).
func (s *Store) PutGeoCache(ctx context.Context, key, value string, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO geo_cache (key, value, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, expiresAt)
	return err
}

// DeleteExpiredGeoCache purges expired geo-cache rows (janitor helper).
func (s *Store) DeleteExpiredGeoCache(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM geo_cache WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
