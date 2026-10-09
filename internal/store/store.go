// Package store persists conversations, messages, and projects in SQLite per
// contract §2 and §8. It uses modernc.org/sqlite (pure Go, driver name
// "sqlite") so the binary builds with CGO_ENABLED=0.
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

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// schema is the DDL from contract §2 plus the projects table (§8.1).
const schema = `
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

// Conversation mirrors a row of the conversations table.
type Conversation struct {
	ID        string  `json:"id"`
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

// Open creates parent directories, opens the database in WAL mode with
// foreign keys enabled, applies the schema, and migrates older databases.
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

// migrate adds columns introduced after the first release without dropping
// data (§8.1): conversations.project_id, messages.reasoning_tokens/duration_ms.
func (s *Store) migrate() error {
	convCols, err := s.columns("conversations")
	if err != nil {
		return err
	}
	if !convCols["project_id"] {
		if _, err := s.db.Exec(
			`ALTER TABLE conversations ADD COLUMN project_id TEXT REFERENCES projects(id) ON DELETE SET NULL`); err != nil {
			return err
		}
	}
	msgCols, err := s.columns("messages")
	if err != nil {
		return err
	}
	if !msgCols["reasoning_tokens"] {
		if _, err := s.db.Exec(`ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER`); err != nil {
			return err
		}
	}
	if !msgCols["duration_ms"] {
		if _, err := s.db.Exec(`ALTER TABLE messages ADD COLUMN duration_ms INTEGER`); err != nil {
			return err
		}
	}
	// Created after the project_id column exists (the schema's CREATE INDEX
	// would fail on a pre-projects database).
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_convos_project ON conversations(project_id)`); err != nil {
		return err
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
func (s *Store) Close() error {
	return s.db.Close()
}

type scanner interface {
	Scan(dest ...any) error
}

// nullString converts a *string to a driver value (nil → NULL).
func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullInt converts a *int64 to a driver value (nil → NULL).
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

const conversationCols = "id, title, model, effort, project_id, created_at, updated_at"

const messageCols = "id, conversation_id, role, content, reasoning, model, " +
	"prompt_tokens, completion_tokens, cached_tokens, cost_micros, " +
	"reasoning_tokens, duration_ms, created_at"

const projectCols = "id, name, instructions, default_model, sort_order, created_at, updated_at"

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

func scanProject(sc scanner) (Project, error) {
	var p Project
	var model sql.NullString
	if err := sc.Scan(&p.ID, &p.Name, &p.Instructions, &model, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Project{}, err
	}
	p.DefaultModel = optString(model)
	return p, nil
}

// ---------- conversations ----------

// InsertConversation inserts a new conversation row.
func (s *Store) InsertConversation(ctx context.Context, c *Conversation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (`+conversationCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Title, c.Model, nullString(c.Effort), nullString(c.ProjectID), c.CreatedAt, c.UpdatedAt)
	return err
}

// GetConversation loads one conversation by id, or ErrNotFound.
func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationCols+` FROM conversations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	return c, err
}

// ListConversations returns all conversations, updated_at DESC.
func (s *Store) ListConversations(ctx context.Context) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationCols+` FROM conversations ORDER BY updated_at DESC`)
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

// UpdateConversation updates title, model, effort, project and updated_at.
func (s *Store) UpdateConversation(ctx context.Context, c *Conversation) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = ?, model = ?, effort = ?, project_id = ?, updated_at = ? WHERE id = ?`,
		c.Title, c.Model, nullString(c.Effort), nullString(c.ProjectID), c.UpdatedAt, c.ID)
	return err
}

// SetTitle updates only the conversation title (used by auto-title).
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	return err
}

// TouchConversation bumps updated_at (last activity).
func (s *Store) TouchConversation(ctx context.Context, id string, updatedAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = ? WHERE id = ?`, updatedAt, id)
	return err
}

// DeleteConversation deletes a conversation; messages cascade-delete.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	return err
}

// DeleteOlderThan deletes conversations whose updated_at is before cutoff
// (unix ms) and returns the number deleted. Used by the retention janitor.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- messages ----------

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
// user message, so a regenerate can replace the final reply. Returns the
// number deleted; 0 with no error when there is no user message.
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

// InsertProject inserts a new project row.
func (s *Store) InsertProject(ctx context.Context, p *Project) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (`+projectCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Instructions, nullString(p.DefaultModel), p.SortOrder, p.CreatedAt, p.UpdatedAt)
	return err
}

// GetProject loads one project by id, or ErrNotFound.
func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	counts, err := s.projectCounts(ctx)
	if err != nil {
		return Project{}, err
	}
	p.ConversationCount = counts[p.ID]
	return p, nil
}

// ListProjects returns all projects with their conversation counts, sorted by
// sort_order then created_at.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectCols+` FROM projects ORDER BY sort_order ASC, created_at ASC`)
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
	counts, err := s.projectCounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		projects[i].ConversationCount = counts[projects[i].ID]
	}
	return projects, nil
}

// UpdateProject updates all mutable fields of a project.
func (s *Store) UpdateProject(ctx context.Context, p *Project) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, instructions = ?, default_model = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Instructions, nullString(p.DefaultModel), p.SortOrder, p.UpdatedAt, p.ID)
	return err
}

// DeleteProject deletes a project; its conversations are moved to Unfiled.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET project_id = NULL WHERE project_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// projectCounts returns the number of conversations per project id.
func (s *Store) projectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, COUNT(1) FROM conversations WHERE project_id IS NOT NULL GROUP BY project_id`)
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
