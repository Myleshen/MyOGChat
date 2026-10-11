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
	"strings"
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
  serper_key_enc TEXT,
  linkwarden_url TEXT,
  linkwarden_token_enc TEXT,
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
  pinned     INTEGER NOT NULL DEFAULT 0,
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
  status     TEXT NOT NULL DEFAULT 'ready',
  error      TEXT,
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
CREATE TABLE IF NOT EXISTS calendar_accounts (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL,      -- caldav | ics
  provider      TEXT,               -- apple | zoho | google | custom
  calendar_id   TEXT,               -- default CalDAV collection path / ICS label
  username      TEXT,               -- CalDAV username (not secret)
  credential    TEXT,               -- encrypted blob: CalDAV password / ICS URL
  endpoint      TEXT,               -- CalDAV server URL / cached home set
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_calendar_accounts_user ON calendar_accounts(user_id);
CREATE TABLE IF NOT EXISTS mail_accounts (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label      TEXT NOT NULL DEFAULT '',
  provider   TEXT NOT NULL,
  email      TEXT NOT NULL,
  imap_host  TEXT NOT NULL,
  imap_port  INTEGER NOT NULL,
  imap_tls   TEXT NOT NULL DEFAULT 'implicit',
  enabled    INTEGER NOT NULL DEFAULT 1,
  is_default INTEGER NOT NULL DEFAULT 0,
  secret_enc TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS images (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  prompt     TEXT NOT NULL,
  model      TEXT,
  mime       TEXT NOT NULL,
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  size       INTEGER NOT NULL,
  data       BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_images_user ON images(user_id);
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
	// Per-user quota overrides (2.11); nil = inherit the global admin setting.
	DailyTokenLimit  *int64  `json:"daily_token_limit,omitempty"`
	MonthlyCostLimit *int64  `json:"monthly_cost_limit,omitempty"`
	SerperKeyEnc     *string `json:"-"` // encrypted per-user Serper key (2.12)
	// Per-user Linkwarden instance (2.8): the URL is not secret, the token is
	// encrypted with internal/secret. Both nil when unconfigured.
	LinkwardenURL      *string `json:"-"`
	LinkwardenTokenEnc *string `json:"-"`
	CreatedAt          int64   `json:"created_at"`
	UpdatedAt          int64   `json:"updated_at"`
}

// Conversation mirrors a row of the conversations table.
type Conversation struct {
	ID        string  `json:"id"`
	UserID    *string `json:"-"`
	Title     string  `json:"title"`
	Model     string  `json:"model"`
	Effort    *string `json:"effort"`
	ProjectID *string `json:"project_id"`
	Pinned    bool    `json:"pinned"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

// ConversationHit is one search result (feature A): a conversation that
// matched the query by title or message content, with a short snippet around
// the first matching message and its last-activity timestamp.
type ConversationHit struct {
	ConversationID string `json:"id"`
	Title          string `json:"title"`
	Snippet        string `json:"snippet"`
	UpdatedAt      int64  `json:"updated_at"`
}

// Message mirrors a row of the messages table.
type Message struct {
	ID               string           `json:"id"`
	ConversationID   string           `json:"conversation_id"`
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	Reasoning        *string          `json:"reasoning"`
	Model            *string          `json:"model"`
	PromptTokens     *int64           `json:"prompt_tokens"`
	CompletionTokens *int64           `json:"completion_tokens"`
	CachedTokens     *int64           `json:"cached_tokens"`
	CostMicros       *int64           `json:"cost_micros"`
	ReasoningTokens  *int64           `json:"reasoning_tokens"`
	DurationMs       *int64           `json:"duration_ms"`
	Attachments      []AttachedFile   `json:"attachments,omitempty"`
	Tools            []ToolActivity   `json:"tools,omitempty"`
	Images           []GeneratedImage `json:"images,omitempty"`
	CreatedAt        int64            `json:"created_at"`
}

// ToolActivity is one tool call's display state persisted as a JSON array on
// messages.tools (2.10), so the inline tool block survives a reload. It mirrors
// the SSE `tool` payload / upstream.ToolActivity. Phase 4.1 also stores the
// tool's arguments, output, error text, wall time, and whether the fields were
// truncated to bound the row size. The full output is only ever shown in the
// UI; the model always receives the uncapped result.
type ToolActivity struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // running | done | error
	Summary    string `json:"summary,omitempty"`
	Args       string `json:"args,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
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

// GeneratedImage is the display metadata of an image generated into a message
// (3.1). The bytes live in the images table; a message stores only a reference
// so history reloads can render it with GET /api/images/{id}.
type GeneratedImage struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Mime   string `json:"mime,omitempty"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Size   int64  `json:"size"`
}

// Image mirrors a row of the images table (3.1). Data holds the PNG bytes;
// it is never serialized to JSON.
type Image struct {
	ID        string `json:"id"`
	UserID    string `json:"-"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	Mime      string `json:"mime"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Size      int64  `json:"size"`
	Data      []byte `json:"-"`
	CreatedAt int64  `json:"created_at"`
}

// Attachment mirrors a row of the attachments table (2.3). Text is the
// extracted plain text folded into the model request. Status tracks async
// extraction (Phase 4.5): "ready" (default), "pending" (media transcription in
// progress), or "error" (extraction failed; Error holds the message).
type Attachment struct {
	ID        string `json:"id"`
	UserID    string `json:"-"`
	Name      string `json:"name"`
	Mime      string `json:"mime,omitempty"`
	Size      int64  `json:"size"`
	Text      string `json:"-"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// Chars reports the number of characters in the extracted text (rune count).
func (a Attachment) Chars() int { return len([]rune(a.Text)) }

// MailAccount mirrors a row of the mail_accounts table (feature 3.2, extended by
// 5.11). A user may have several mailboxes; exactly one is the default
// (is_default = 1, enforced by a partial unique index). SecretEnc is the
// AES-256-GCM encrypted JSON blob holding the app-specific password; it is never
// returned to the SPA.
type MailAccount struct {
	ID        string `json:"id"`
	UserID    string `json:"-"`
	Label     string `json:"label"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	Host      string `json:"imap_host"`
	Port      int    `json:"imap_port"`
	TLS       string `json:"imap_tls"`
	Enabled   bool   `json:"enabled"`
	IsDefault bool   `json:"is_default"`
	SecretEnc string `json:"-"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
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
	if err := s.migrateMailAccounts(); err != nil {
		return err
	}
	steps := []struct {
		table, column, ddl string
	}{
		{"conversations", "project_id", `ALTER TABLE conversations ADD COLUMN project_id TEXT REFERENCES projects(id) ON DELETE SET NULL`},
		{"messages", "reasoning_tokens", `ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER`},
		{"messages", "duration_ms", `ALTER TABLE messages ADD COLUMN duration_ms INTEGER`},
		{"messages", "attachments", `ALTER TABLE messages ADD COLUMN attachments TEXT`},
		{"messages", "tools", `ALTER TABLE messages ADD COLUMN tools TEXT`},
		{"conversations", "user_id", `ALTER TABLE conversations ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`},
		{"projects", "user_id", `ALTER TABLE projects ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`},
		{"users", "status", `ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`},
		{"users", "api_key_enc", `ALTER TABLE users ADD COLUMN api_key_enc TEXT`},
		{"users", "daily_token_limit", `ALTER TABLE users ADD COLUMN daily_token_limit INTEGER`},
		{"users", "monthly_cost_limit", `ALTER TABLE users ADD COLUMN monthly_cost_limit INTEGER`},
		{"users", "serper_key_enc", `ALTER TABLE users ADD COLUMN serper_key_enc TEXT`},
		{"users", "linkwarden_url", `ALTER TABLE users ADD COLUMN linkwarden_url TEXT`},
		{"users", "linkwarden_token_enc", `ALTER TABLE users ADD COLUMN linkwarden_token_enc TEXT`},
		{"messages", "images", `ALTER TABLE messages ADD COLUMN images TEXT`},
		{"attachments", "status", `ALTER TABLE attachments ADD COLUMN status TEXT NOT NULL DEFAULT 'ready'`},
		{"attachments", "error", `ALTER TABLE attachments ADD COLUMN error TEXT`},
		// Pinned chats (feature B): additive, defaults to unpinned.
		{"conversations", "pinned", `ALTER TABLE conversations ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`},
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
		// 2.11: range scans over assistant usage rows.
		`CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_geo_cache_expires ON geo_cache(expires_at)`,
		// 5.11: multi-mailbox lookups + at most one default per user.
		`CREATE INDEX IF NOT EXISTS idx_mail_accounts_user ON mail_accounts(user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_mail_accounts_default ON mail_accounts(user_id) WHERE is_default = 1`,
	} {
		if _, err := s.db.Exec(idx); err != nil {
			return err
		}
	}
	return nil
}

// migrateMailAccounts rebuilds a pre-5.11 mail_accounts table (one mailbox per
// user, user_id PRIMARY KEY) into the multi-mailbox shape. SQLite cannot alter a
// PRIMARY KEY, so the table is rebuilt in one transaction: the existing row is
// copied verbatim (ciphertext included) and marked is_default = 1 with its email
// address as the label. It is guarded by "does id exist?": fresh installs already
// have the new shape and skip the rebuild entirely.
func (s *Store) migrateMailAccounts() error {
	cols, err := s.columns("mail_accounts")
	if err != nil {
		return err
	}
	if cols["id"] {
		return nil
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`ALTER TABLE mail_accounts RENAME TO mail_accounts_legacy`,
		`CREATE TABLE mail_accounts (
		   id         TEXT PRIMARY KEY,
		   user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		   label      TEXT NOT NULL DEFAULT '',
		   provider   TEXT NOT NULL,
		   email      TEXT NOT NULL,
		   imap_host  TEXT NOT NULL,
		   imap_port  INTEGER NOT NULL,
		   imap_tls   TEXT NOT NULL DEFAULT 'implicit',
		   enabled    INTEGER NOT NULL DEFAULT 1,
		   is_default INTEGER NOT NULL DEFAULT 0,
		   secret_enc TEXT NOT NULL,
		   created_at INTEGER NOT NULL,
		   updated_at INTEGER NOT NULL
		 )`,
		`INSERT INTO mail_accounts
		   (id, user_id, label, provider, email, imap_host, imap_port, imap_tls, enabled, is_default, secret_enc, created_at, updated_at)
		 SELECT lower(hex(randomblob(16))), user_id, email, provider, email, imap_host, imap_port, imap_tls, enabled, 1, secret_enc, created_at, updated_at
		   FROM mail_accounts_legacy`,
		`DROP TABLE mail_accounts_legacy`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// nullText maps an empty string to SQL NULL (used for the non-secret per-user
// Linkwarden URL, so a cleared field reads back as nil).
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
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

const userCols = "id, username, email, password_hash, is_admin, status, api_key_enc, daily_token_limit, monthly_cost_limit, serper_key_enc, linkwarden_url, linkwarden_token_enc, created_at, updated_at"

func scanUser(sc scanner) (User, error) {
	var u User
	var admin int
	var keyEnc sql.NullString
	var daily, monthly sql.NullInt64
	var serperEnc, linkURL, linkTokenEnc sql.NullString
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &admin, &u.Status, &keyEnc, &daily, &monthly, &serperEnc, &linkURL, &linkTokenEnc, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, err
	}
	u.IsAdmin = admin != 0
	u.APIKeyEnc = optString(keyEnc)
	u.DailyTokenLimit = optInt(daily)
	u.MonthlyCostLimit = optInt(monthly)
	u.SerperKeyEnc = optString(serperEnc)
	u.LinkwardenURL = optString(linkURL)
	u.LinkwardenTokenEnc = optString(linkTokenEnc)
	return u, nil
}

// InsertUser inserts a new user row; duplicate username/email surface as a
// driver error (the caller maps it to 409).
func (s *Store) InsertUser(ctx context.Context, u *User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.Email, u.PasswordHash, boolToInt(u.IsAdmin), u.Status, nullString(u.APIKeyEnc),
		nullInt(u.DailyTokenLimit), nullInt(u.MonthlyCostLimit), nullString(u.SerperKeyEnc),
		nullString(u.LinkwardenURL), nullString(u.LinkwardenTokenEnc), u.CreatedAt, u.UpdatedAt)
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

// SetUserLimits stores (or clears, when nil) a user's per-user quota overrides
// (2.11). A nil override inherits the global admin setting.
func (s *Store) SetUserLimits(ctx context.Context, id string, daily, monthly *int64, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET daily_token_limit = ?, monthly_cost_limit = ?, updated_at = ? WHERE id = ?`,
		nullInt(daily), nullInt(monthly), now, id)
	return err
}

// SetUserSerperKey stores (or clears, when nil) a user's encrypted Serper key
// (2.12).
func (s *Store) SetUserSerperKey(ctx context.Context, id string, enc *string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET serper_key_enc = ?, updated_at = ? WHERE id = ?`, nullString(enc), now, id)
	return err
}

// SetUserLinkwarden stores (or clears) a user's per-user Linkwarden instance
// (2.8). An empty url clears the URL; a nil tokenEnc clears the token. Both
// fields are always written together so callers should pass the desired state.
func (s *Store) SetUserLinkwarden(ctx context.Context, id, url string, tokenEnc *string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET linkwarden_url = ?, linkwarden_token_enc = ?, updated_at = ? WHERE id = ?`,
		nullText(url), nullString(tokenEnc), now, id)
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

const conversationCols = "id, user_id, title, model, effort, project_id, pinned, created_at, updated_at"

func scanConversation(sc scanner) (Conversation, error) {
	var c Conversation
	var userID, effort, project sql.NullString
	var pinned int
	if err := sc.Scan(&c.ID, &userID, &c.Title, &c.Model, &effort, &project, &pinned, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Conversation{}, err
	}
	c.UserID = optString(userID)
	c.Effort = optString(effort)
	c.ProjectID = optString(project)
	c.Pinned = pinned != 0
	return c, nil
}

// InsertConversation inserts a new conversation row for its owner.
func (s *Store) InsertConversation(ctx context.Context, c *Conversation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, user_id, title, model, effort, project_id, pinned, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, nullString(c.UserID), c.Title, c.Model, nullString(c.Effort), nullString(c.ProjectID), boolToInt(c.Pinned), c.CreatedAt, c.UpdatedAt)
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

// ListConversations returns the user's conversations, pinned first, then
// updated_at DESC.
func (s *Store) ListConversations(ctx context.Context, userID string) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationCols+` FROM conversations WHERE user_id = ? ORDER BY pinned DESC, updated_at DESC`, userID)
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

// SetConversationPinned pins or unpins one of the user's conversations. It
// returns ErrNotFound when the row is missing or not owned by userID.
func (s *Store) SetConversationPinned(ctx context.Context, id, userID string, pinned bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET pinned = ? WHERE id = ? AND user_id = ?`,
		boolToInt(pinned), id, userID)
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

// SearchConversations matches the user's conversations by title or message
// content (case-insensitive) and returns up to limit hits, most recently
// active first. Each hit carries a ~120-char snippet centred on the first
// matching message; a title-only match has an empty snippet.
func (s *Store) SearchConversations(ctx context.Context, userID, query string, limit int) ([]ConversationHit, error) {
	hits := make([]ConversationHit, 0)
	query = strings.TrimSpace(query)
	if query == "" {
		return hits, nil
	}
	if limit <= 0 {
		limit = 50
	}
	like := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.title, c.updated_at,
		        (SELECT m.content FROM messages m
		          WHERE m.conversation_id = c.id AND lower(m.content) LIKE ?
		          ORDER BY m.created_at ASC, m.rowid ASC LIMIT 1)
		   FROM conversations c
		  WHERE c.user_id = ?
		    AND (lower(c.title) LIKE ?
		         OR EXISTS (SELECT 1 FROM messages m
		                     WHERE m.conversation_id = c.id AND lower(m.content) LIKE ?))
		  ORDER BY c.updated_at DESC
		  LIMIT ?`,
		like, userID, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, title string
			updatedAt int64
			content   sql.NullString
		)
		if err := rows.Scan(&id, &title, &updatedAt, &content); err != nil {
			return nil, err
		}
		var snippet string
		if content.Valid && content.String != "" {
			snippet = makeSnippet(content.String, query, 120)
		}
		hits = append(hits, ConversationHit{
			ConversationID: id,
			Title:          title,
			Snippet:        snippet,
			UpdatedAt:      updatedAt,
		})
	}
	return hits, rows.Err()
}

// makeSnippet returns up to width runes of content around the first
// case-insensitive occurrence of query, collapsing newlines and marking
// truncation with an ellipsis. With no match it returns the leading runes.
func makeSnippet(content, query string, width int) string {
	if width <= 0 {
		width = 120
	}
	content = strings.TrimSpace(strings.Join(strings.Fields(content), " "))
	runes := []rune(content)
	if len(runes) == 0 {
		return ""
	}
	match := -1
	if query != "" {
		if idx := strings.Index(strings.ToLower(content), strings.ToLower(query)); idx >= 0 {
			match = len([]rune(content[:idx]))
		}
	}
	if match < 0 || len(runes) <= width {
		if match < 0 && len(runes) > width {
			return string(runes[:width]) + "…"
		}
		return string(runes)
	}
	qlen := len([]rune(query))
	start := match - (width-qlen)/2
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		start = end - width
		if start < 0 {
			start = 0
		}
	}
	s := string(runes[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
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
	"reasoning_tokens, duration_ms, attachments, tools, images, created_at"

func scanMessage(sc scanner) (Message, error) {
	var m Message
	var reasoning, model, attachments, tools, images sql.NullString
	var promptTokens, completionTokens, cachedTokens, costMicros sql.NullInt64
	var reasoningTokens, durationMs sql.NullInt64
	err := sc.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content,
		&reasoning, &model, &promptTokens, &completionTokens, &cachedTokens,
		&costMicros, &reasoningTokens, &durationMs, &attachments, &tools, &images, &m.CreatedAt)
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
	if tools.Valid && tools.String != "" {
		// Same contract as attachments: malformed JSON is dropped, not fatal.
		_ = json.Unmarshal([]byte(tools.String), &m.Tools)
	}
	if images.Valid && images.String != "" {
		_ = json.Unmarshal([]byte(images.String), &m.Images)
	}
	return m, nil
}

// InsertMessage inserts a new message row.
func (s *Store) InsertMessage(ctx context.Context, m *Message) error {
	attachments, err := marshalJSON(m.Attachments)
	if err != nil {
		return err
	}
	tools, err := marshalJSON(m.Tools)
	if err != nil {
		return err
	}
	images, err := marshalJSON(m.Images)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO messages (`+messageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, m.Role, m.Content, nullString(m.Reasoning),
		nullString(m.Model), nullInt(m.PromptTokens), nullInt(m.CompletionTokens),
		nullInt(m.CachedTokens), nullInt(m.CostMicros), nullInt(m.ReasoningTokens),
		nullInt(m.DurationMs), attachments, tools, images, m.CreatedAt)
	return err
}

// marshalJSON encodes a non-empty slice as a JSON string for a TEXT column,
// returning nil (SQL NULL) for an empty slice.
func marshalJSON[T any](v []T) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
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

// EditUserMessage updates the content of one user message. It only matches a
// row that is in conversationID, is owned by userID, and has role 'user';
// anything else returns ErrNotFound (so a cross-user or non-user target is
// indistinguishable from a missing row).
func (s *Store) EditUserMessage(ctx context.Context, conversationID, messageID, userID, content string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET content = ?
		  WHERE id = ? AND conversation_id = ? AND role = 'user'
		    AND conversation_id IN (SELECT id FROM conversations WHERE id = ? AND user_id = ?)`,
		content, messageID, conversationID, conversationID, userID)
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

// TruncateAfter deletes every message that follows messageID in the
// conversation, using the same (created_at, rowid) ordering as ListMessages. A
// missing messageID matches nothing, so it is a no-op.
func (s *Store) TruncateAfter(ctx context.Context, conversationID, messageID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM messages
		  WHERE conversation_id = ?
		    AND (created_at, rowid) > (
		      SELECT created_at, rowid FROM messages WHERE id = ? AND conversation_id = ?)`,
		conversationID, messageID, conversationID)
	return err
}

// EditUserMessageAndTruncate rewrites a user message and deletes every message
// after it, in one transaction, so a failed truncate can never leave an edited
// prompt with its stale reply (Phase 5 hardening). Owner-scoped: a missing /
// non-user / cross-user target is ErrNotFound.
func (s *Store) EditUserMessageAndTruncate(ctx context.Context, conversationID, messageID, userID, content string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET content = ?
		  WHERE id = ? AND conversation_id = ? AND role = 'user'
		    AND conversation_id IN (SELECT id FROM conversations WHERE id = ? AND user_id = ?)`,
		content, messageID, conversationID, conversationID, userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages
		  WHERE conversation_id = ?
		    AND (created_at, rowid) > (
		      SELECT created_at, rowid FROM messages WHERE id = ? AND conversation_id = ?)`,
		conversationID, messageID, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

// Backup writes a consistent snapshot of the live database to destPath using
// SQLite's VACUUM INTO (which fails if destPath exists). It is used by the
// admin backup endpoint (§19) to hand an operator a point-in-time copy of the
// DB, including any WAL content.
func (s *Store) Backup(ctx context.Context, destPath string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destPath)
	return err
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

const attachmentCols = "id, user_id, name, mime, size, text, status, error, created_at"

func scanAttachment(sc scanner) (Attachment, error) {
	var a Attachment
	var mime, status, errMsg sql.NullString
	if err := sc.Scan(&a.ID, &a.UserID, &a.Name, &mime, &a.Size, &a.Text, &status, &errMsg, &a.CreatedAt); err != nil {
		return Attachment{}, err
	}
	if mime.Valid {
		a.Mime = mime.String
	}
	a.Status = "ready"
	if status.Valid && status.String != "" {
		a.Status = status.String
	}
	if errMsg.Valid {
		a.Error = errMsg.String
	}
	return a, nil
}

// InsertAttachment stores an extracted file for its owner. An empty Status
// defaults to "ready" (synchronous extraction); media uploads pass "pending".
func (s *Store) InsertAttachment(ctx context.Context, a *Attachment) error {
	if a.Status == "" {
		a.Status = "ready"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO attachments (`+attachmentCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.Name, nullString(&a.Mime), a.Size, a.Text, a.Status, nullString(&a.Error), a.CreatedAt)
	return err
}

// SetAttachmentResult finalizes an asynchronously extracted attachment. It is
// not owner-scoped because the background worker runs without a request user.
func (s *Store) SetAttachmentResult(ctx context.Context, id, text, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE attachments SET text = ?, status = ?, error = ? WHERE id = ?`,
		text, status, nullString(&errMsg), id)
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

// ---------- usage (2.11) ----------

// UsageTotals aggregates token and cost counters over assistant messages.
type UsageTotals struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CostMicros       int64 `json:"cost_micros"`
	Messages         int64 `json:"messages"`
}

// UsageRow is one by-day or by-model bucket. Exactly one of Day/Model is set.
type UsageRow struct {
	Day   string `json:"day,omitempty"`
	Model string `json:"model,omitempty"`
	UsageTotals
}

// UserUsage is one user's totals for the admin view.
type UserUsage struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	UsageTotals
}

// usageSumCols is the shared aggregate projection over an aliased `messages m`.
const usageSumCols = `COALESCE(SUM(m.prompt_tokens),0), COALESCE(SUM(m.completion_tokens),0), ` +
	`COALESCE(SUM(m.cached_tokens),0), COALESCE(SUM(m.reasoning_tokens),0), ` +
	`COALESCE(SUM(m.cost_micros),0), COUNT(1)`

// usageScope is the shared WHERE clause: assistant rows of the owner's
// conversations, optionally restricted to created_at >= cutoff (0 = all time).
const usageScope = `m.role = 'assistant' AND (? = 0 OR m.created_at >= ?)`

// UserUsageTotals sums one user's assistant usage since cutoff (unix ms; 0 =
// all time).
func (s *Store) UserUsageTotals(ctx context.Context, userID string, cutoff int64) (UsageTotals, error) {
	var t UsageTotals
	err := s.db.QueryRowContext(ctx,
		`SELECT `+usageSumCols+`
		   FROM messages m JOIN conversations c ON c.id = m.conversation_id
		  WHERE c.user_id = ? AND `+usageScope,
		userID, cutoff, cutoff).Scan(
		&t.PromptTokens, &t.CompletionTokens, &t.CachedTokens,
		&t.ReasoningTokens, &t.CostMicros, &t.Messages)
	if err != nil {
		return UsageTotals{}, err
	}
	return t, nil
}

// UserUsageByDay buckets one user's assistant usage by UTC calendar day,
// oldest first.
func (s *Store) UserUsageByDay(ctx context.Context, userID string, cutoff int64) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', m.created_at/1000, 'unixepoch') AS day, `+usageSumCols+`
		   FROM messages m JOIN conversations c ON c.id = m.conversation_id
		  WHERE c.user_id = ? AND `+usageScope+`
		  GROUP BY day ORDER BY day ASC`,
		userID, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UsageRow, 0)
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.PromptTokens, &r.CompletionTokens,
			&r.CachedTokens, &r.ReasoningTokens, &r.CostMicros, &r.Messages); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserUsageByModel buckets one user's assistant usage by model, descending by
// cost then tokens.
func (s *Store) UserUsageByModel(ctx context.Context, userID string, cutoff int64) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(m.model,'') AS model, `+usageSumCols+`
		   FROM messages m JOIN conversations c ON c.id = m.conversation_id
		  WHERE c.user_id = ? AND `+usageScope+`
		  GROUP BY m.model
		  ORDER BY SUM(COALESCE(m.cost_micros,0)) DESC, SUM(COALESCE(m.prompt_tokens,0)+COALESCE(m.completion_tokens,0)) DESC`,
		userID, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UsageRow, 0)
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Model, &r.PromptTokens, &r.CompletionTokens,
			&r.CachedTokens, &r.ReasoningTokens, &r.CostMicros, &r.Messages); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllUsersUsage sums every user's assistant usage since cutoff (unix ms; 0 =
// all time) for the admin view.
func (s *Store) AllUsersUsage(ctx context.Context, cutoff int64) ([]UserUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.user_id, COALESCE(u.username,''), `+usageSumCols+`
		   FROM messages m
		   JOIN conversations c ON c.id = m.conversation_id
		   LEFT JOIN users u ON u.id = c.user_id
		  WHERE `+usageScope+`
		  GROUP BY c.user_id ORDER BY u.username ASC`,
		cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UserUsage, 0)
	for rows.Next() {
		var u UserUsage
		if err := rows.Scan(&u.UserID, &u.Username, &u.PromptTokens,
			&u.CompletionTokens, &u.CachedTokens, &u.ReasoningTokens,
			&u.CostMicros, &u.Messages); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
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

// ---------- images (3.1) ----------

const imageCols = "id, user_id, prompt, model, mime, width, height, size, data, created_at"

func scanImage(sc scanner) (Image, error) {
	var img Image
	var model sql.NullString
	if err := sc.Scan(&img.ID, &img.UserID, &img.Prompt, &model, &img.Mime,
		&img.Width, &img.Height, &img.Size, &img.Data, &img.CreatedAt); err != nil {
		return Image{}, err
	}
	if model.Valid {
		img.Model = model.String
	}
	return img, nil
}

// InsertImage stores a generated image (bytes + metadata) for its owner.
func (s *Store) InsertImage(ctx context.Context, img *Image) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO images (`+imageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.UserID, img.Prompt, nullString(&img.Model), img.Mime,
		img.Width, img.Height, img.Size, img.Data, img.CreatedAt)
	return err
}

// GetImage loads one image (including bytes) owned by userID, else ErrNotFound.
// The owner guard makes cross-user access indistinguishable from a missing row.
func (s *Store) GetImage(ctx context.Context, id, userID string) (Image, error) {
	img, err := scanImage(s.db.QueryRowContext(ctx,
		`SELECT `+imageCols+` FROM images WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	return img, err
}

// DeleteImage removes an owner's image. It returns the number of rows removed
// (0 when not owned by userID).
func (s *Store) DeleteImage(ctx context.Context, id, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- calendar accounts (3.3) ----------

// CalendarAccount is one connected calendar per user. Kind is "caldav" or
// "ics"; CredentialEnc holds the encrypted secret (CalDAV password or the
// secret ICS URL) and is never returned to the client.
type CalendarAccount struct {
	ID            string `json:"id"`
	UserID        string `json:"-"`
	Kind          string `json:"kind"`
	Provider      string `json:"provider,omitempty"`
	CalendarID    string `json:"calendar_id,omitempty"`
	Username      string `json:"username,omitempty"`
	CredentialEnc string `json:"-"`
	Endpoint      string `json:"endpoint,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

const calendarAccountCols = "id, user_id, kind, provider, calendar_id, username, credential, endpoint, created_at, updated_at"

func scanCalendarAccount(sc scanner) (CalendarAccount, error) {
	var a CalendarAccount
	var provider, calendarID, username, credential, endpoint sql.NullString
	if err := sc.Scan(&a.ID, &a.UserID, &a.Kind, &provider, &calendarID, &username, &credential, &endpoint, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return CalendarAccount{}, err
	}
	a.Provider = strOr(provider)
	a.CalendarID = strOr(calendarID)
	a.Username = strOr(username)
	a.CredentialEnc = strOr(credential)
	a.Endpoint = strOr(endpoint)
	return a, nil
}

func strOr(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}

// GetCalendarAccount loads a user's connected calendar, else ErrNotFound.
func (s *Store) GetCalendarAccount(ctx context.Context, userID string) (CalendarAccount, error) {
	a, err := scanCalendarAccount(s.db.QueryRowContext(ctx,
		`SELECT `+calendarAccountCols+` FROM calendar_accounts WHERE user_id = ?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return CalendarAccount{}, ErrNotFound
	}
	return a, err
}

// ---------- mail accounts (feature 3.2, multi-mailbox 5.11) ----------

const mailAccountCols = "id, user_id, label, provider, email, imap_host, imap_port, imap_tls, enabled, is_default, secret_enc, created_at, updated_at"

// MaxMailAccounts caps how many mailboxes one user may connect (runaway guard).
const MaxMailAccounts = 10

func scanMailAccount(sc scanner) (MailAccount, error) {
	var a MailAccount
	var enabled, isDefault int
	if err := sc.Scan(&a.ID, &a.UserID, &a.Label, &a.Provider, &a.Email, &a.Host, &a.Port, &a.TLS, &enabled, &isDefault, &a.SecretEnc, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return MailAccount{}, err
	}
	a.Enabled = enabled != 0
	a.IsDefault = isDefault != 0
	return a, nil
}

// ListMailAccounts returns the user's mailboxes, default first then oldest first.
func (s *Store) ListMailAccounts(ctx context.Context, userID string) ([]MailAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mailAccountCols+` FROM mail_accounts WHERE user_id = ?
		  ORDER BY is_default DESC, created_at ASC, id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailAccount
	for rows.Next() {
		a, err := scanMailAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountMailAccounts reports how many mailboxes the user has connected.
func (s *Store) CountMailAccounts(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_accounts WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// GetMailAccount loads the user's default mailbox, else ErrNotFound. It is the
// resolution behind the singular /api/me/mail aliases.
func (s *Store) GetMailAccount(ctx context.Context, userID string) (MailAccount, error) {
	a, err := scanMailAccount(s.db.QueryRowContext(ctx,
		`SELECT `+mailAccountCols+` FROM mail_accounts WHERE user_id = ? AND is_default = 1`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return MailAccount{}, ErrNotFound
	}
	return a, err
}

// GetMailAccountByID loads one of the user's mailboxes by id, else ErrNotFound.
func (s *Store) GetMailAccountByID(ctx context.Context, userID, id string) (MailAccount, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return MailAccount{}, ErrNotFound
	}
	a, err := scanMailAccount(s.db.QueryRowContext(ctx,
		`SELECT `+mailAccountCols+` FROM mail_accounts WHERE user_id = ? AND id = ?`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MailAccount{}, ErrNotFound
	}
	return a, err
}

// ResolveMailAccount resolves an account reference: an empty ref is the user's
// default mailbox; otherwise the id (exact) or a case-insensitive label match.
// It is owner-scoped; a cross-user or unknown ref is ErrNotFound.
func (s *Store) ResolveMailAccount(ctx context.Context, userID, ref string) (MailAccount, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return s.GetMailAccount(ctx, userID)
	}
	if a, err := s.GetMailAccountByID(ctx, userID, ref); err == nil {
		return a, nil
	} else if !errors.Is(err, ErrNotFound) {
		return MailAccount{}, err
	}
	a, err := scanMailAccount(s.db.QueryRowContext(ctx,
		`SELECT `+mailAccountCols+`
		   FROM mail_accounts
		  WHERE user_id = ? AND label <> '' AND lower(label) = lower(?)
		  ORDER BY is_default DESC, created_at ASC, id ASC
		  LIMIT 1`, userID, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return MailAccount{}, ErrNotFound
	}
	return a, err
}

// CreateMailAccount inserts a mailbox for a user. The first mailbox becomes the
// default; an explicit IsDefault clears the previous default in the same
// transaction (the partial unique index allows only one). The id is generated in
// SQL; on return a.ID and a.IsDefault reflect the stored row.
func (s *Store) CreateMailAccount(ctx context.Context, a *MailAccount) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_accounts WHERE user_id = ?`, a.UserID).Scan(&count); err != nil {
		return err
	}
	isDefault := a.IsDefault || count == 0
	if isDefault {
		if _, err := tx.ExecContext(ctx, `UPDATE mail_accounts SET is_default = 0 WHERE user_id = ?`, a.UserID); err != nil {
			return err
		}
	}
	if a.ID == "" {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO mail_accounts
			   (id, user_id, label, provider, email, imap_host, imap_port, imap_tls, enabled, is_default, secret_enc, created_at, updated_at)
			 VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 RETURNING id`,
			a.UserID, a.Label, a.Provider, a.Email, a.Host, a.Port, a.TLS,
			boolToInt(a.Enabled), boolToInt(isDefault), a.SecretEnc, a.CreatedAt, a.UpdatedAt).Scan(&a.ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mail_accounts
			   (id, user_id, label, provider, email, imap_host, imap_port, imap_tls, enabled, is_default, secret_enc, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.UserID, a.Label, a.Provider, a.Email, a.Host, a.Port, a.TLS,
			boolToInt(a.Enabled), boolToInt(isDefault), a.SecretEnc, a.CreatedAt, a.UpdatedAt); err != nil {
			return err
		}
	}
	a.IsDefault = isDefault
	return tx.Commit()
}

// UpdateMailAccount updates a mailbox by (id, user). A cross-user or unknown id
// is ErrNotFound.
func (s *Store) UpdateMailAccount(ctx context.Context, a *MailAccount) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mail_accounts
		    SET label = ?, provider = ?, email = ?, imap_host = ?, imap_port = ?, imap_tls = ?, enabled = ?, secret_enc = ?, updated_at = ?
		  WHERE id = ? AND user_id = ?`,
		a.Label, a.Provider, a.Email, a.Host, a.Port, a.TLS, boolToInt(a.Enabled), a.SecretEnc, a.UpdatedAt, a.ID, a.UserID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMailAccountEnabled flips one mailbox's email-tools toggle.
func (s *Store) SetMailAccountEnabled(ctx context.Context, userID, id string, enabled bool, now int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mail_accounts SET enabled = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		boolToInt(enabled), now, userID, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDefaultMailAccount makes one mailbox the user's default in a transaction,
// clearing the previous default first so the partial unique index is satisfied.
func (s *Store) SetDefaultMailAccount(ctx context.Context, userID, id string, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE mail_accounts SET is_default = 0 WHERE user_id = ? AND is_default = 1 AND id <> ?`, userID, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE mail_accounts SET is_default = 1, updated_at = ? WHERE user_id = ? AND id = ?`, now, userID, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DeleteMailAccount disconnects (and deletes the credential of) one mailbox in a
// transaction. When the deleted mailbox was the default, the oldest remaining
// mailbox is promoted so a user with mailboxes always has a default.
func (s *Store) DeleteMailAccount(ctx context.Context, userID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var wasDefault int
	err = tx.QueryRowContext(ctx,
		`SELECT is_default FROM mail_accounts WHERE user_id = ? AND id = ?`, userID, id).Scan(&wasDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_accounts WHERE user_id = ? AND id = ?`, userID, id); err != nil {
		return err
	}
	if wasDefault != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mail_accounts SET is_default = 1
			  WHERE id = (SELECT id FROM mail_accounts WHERE user_id = ? ORDER BY created_at ASC, id ASC LIMIT 1)`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertCalendarAccount inserts or replaces the user's calendar account
// (one per user). created_at is preserved on update.
func (s *Store) UpsertCalendarAccount(ctx context.Context, a *CalendarAccount) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO calendar_accounts (`+calendarAccountCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   kind = excluded.kind,
		   provider = excluded.provider,
		   calendar_id = excluded.calendar_id,
		   username = excluded.username,
		   credential = excluded.credential,
		   endpoint = excluded.endpoint,
		   updated_at = excluded.updated_at`,
		a.ID, a.UserID, a.Kind, nullString(&a.Provider), nullString(&a.CalendarID),
		nullString(&a.Username), nullString(&a.CredentialEnc), nullString(&a.Endpoint),
		a.CreatedAt, a.UpdatedAt)
	return err
}

// DeleteCalendarAccount removes a user's connected calendar; returns rows
// removed (0 when none).
func (s *Store) DeleteCalendarAccount(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM calendar_accounts WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteImagesOlderThan purges images created before cutoff (unix ms) across
// all users, for the retention janitor.
func (s *Store) DeleteImagesOlderThan(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
