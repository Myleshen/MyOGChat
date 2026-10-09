// Package store persists users, settings, conversations, messages, and
// projects in SQLite per contract §2, §8, and §9. It uses modernc.org/sqlite
// (pure Go, driver name "sqlite") so the binary builds with CGO_ENABLED=0.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
CREATE INDEX IF NOT EXISTS idx_messages_convo ON messages(conversation_id, created_at);
CREATE INDEX IF NOT EXISTS idx_convos_updated ON conversations(updated_at DESC);
`

// User mirrors a row of the users table (§9.1).
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	Status       string `json:"status"` // active | pending | disabled
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
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
	ID               string  `json:"id"`
	ConversationID   string  `json:"conversation_id"`
	Role             string  `json:"role"`
	Content          string  `json:"content"`
	Reasoning        *string `json:"reasoning"`
	Model            *string `json:"model"`
	PromptTokens     *int64  `json:"prompt_tokens"`
	CompletionTokens *int64  `json:"completion_tokens"`
	CachedTokens     *int64  `json:"cached_tokens"`
	CostMicros       *int64  `json:"cost_micros"`
	ReasoningTokens  *int64  `json:"reasoning_tokens"`
	DurationMs       *int64  `json:"duration_ms"`
	CreatedAt        int64   `json:"created_at"`
}

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
		{"conversations", "user_id", `ALTER TABLE conversations ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`},
		{"projects", "user_id", `ALTER TABLE projects ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`},
		{"users", "status", `ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`},
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

const userCols = "id, username, email, password_hash, is_admin, status, created_at, updated_at"

func scanUser(sc scanner) (User, error) {
	var u User
	var admin int
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &admin, &u.Status, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, err
	}
	u.IsAdmin = admin != 0
	return u, nil
}

// InsertUser inserts a new user row; duplicate username/email surface as a
// driver error (the caller maps it to 409).
func (s *Store) InsertUser(ctx context.Context, u *User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.Email, u.PasswordHash, boolToInt(u.IsAdmin), u.Status, u.CreatedAt, u.UpdatedAt)
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
	"reasoning_tokens, duration_ms, created_at"

func scanMessage(sc scanner) (Message, error) {
	var m Message
	var reasoning, model sql.NullString
	var promptTokens, completionTokens, cachedTokens, costMicros sql.NullInt64
	var reasoningTokens, durationMs sql.NullInt64
	err := sc.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content,
		&reasoning, &model, &promptTokens, &completionTokens, &cachedTokens,
		&costMicros, &reasoningTokens, &durationMs, &m.CreatedAt)
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
	return m, nil
}

// InsertMessage inserts a new message row.
func (s *Store) InsertMessage(ctx context.Context, m *Message) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (`+messageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, m.Role, m.Content, nullString(m.Reasoning),
		nullString(m.Model), nullInt(m.PromptTokens), nullInt(m.CompletionTokens),
		nullInt(m.CachedTokens), nullInt(m.CostMicros), nullInt(m.ReasoningTokens),
		nullInt(m.DurationMs), m.CreatedAt)
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
