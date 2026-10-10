// Command myogchat serves the MyOGChat single-page app and its API from one
// binary: embedded frontend, SQLite storage, multi-user auth, and an OpenCode
// Go upstream proxy.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/ocr"
	"myogchat/internal/sandbox"
	"myogchat/internal/server"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// all:static embeds the whole frontend directory, including files whose names
// begin with "_" or "." (embed's default would skip those).
//
//go:embed all:static
var staticFS embed.FS

// sessionSecretKey is the settings key holding the cookie-signing secret (§9.1).
const sessionSecretKey = "session_secret"

// encryptionKey is the settings key holding the AES-256 key used to encrypt
// per-user API keys at rest (§2.1).
const encryptionKey = "encryption_key"

// config is the resolved runtime configuration (DESIGN.md).
type config struct {
	apiKey           string
	tavilyKey        string
	braveKey         string
	searchProvider   string
	searchResults    int
	searchRegion     string
	searchSafeSearch string
	port             string
	dbPath           string
	retentionDays    int
	sandboxEnabled   bool           // MYOGCHAT_SANDBOX hard kill switch (default on)
	sandbox          sandbox.Config // code-sandbox container parameters (2.6/2.7)
	// Image OCR (feature 2.5).
	ocrEnabled bool
	ocrImage   string
	ocrLangs   string
	ocrDocker  string
	ocrTimeout time.Duration
	// Self-hosted image generation (feature 3.1).
	imagegenURL   string
	imagegenToken string
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// `myogchat user <add|rm|list>` manages accounts directly in the database,
	// so no admin credentials are needed (handy for test accounts).
	if len(os.Args) > 1 && os.Args[1] == "user" {
		if err := runUserCmd(os.Args[2:]); err != nil {
			log.Error("user command failed", "error", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Error("configuration error", "error", err)
		os.Exit(1)
	}

	if err := run(cfg, log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// resolveDBPath returns MYOGCHAT_DB or the default database path.
func resolveDBPath() (string, error) {
	if v := os.Getenv("MYOGCHAT_DB"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "myogchat", "chat.db"), nil
}

// runUserCmd implements `myogchat user <add|rm|list>` against the database.
func runUserCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: myogchat user <add|rm|list>")
	}
	path, err := resolveDBPath()
	if err != nil {
		return err
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch args[0] {
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Printf("%-16s %-28s admin=%-5v status=%s\n", u.Username, u.Email, u.IsAdmin, u.Status)
		}
		return nil

	case "add":
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		username := fs.String("username", "", "username")
		email := fs.String("email", "", "email")
		password := fs.String("password", "", "password")
		admin := fs.Bool("admin", false, "grant admin")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *username == "" || *email == "" || *password == "" {
			return errors.New("--username, --email and --password are required")
		}
		if msg, ok := auth.ValidateCredentials(*username, *email, *password); !ok {
			return errors.New(msg)
		}
		hash, err := auth.HashPassword(*password)
		if err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		u := store.User{
			ID: uuid(), Username: *username, Email: *email, PasswordHash: hash,
			IsAdmin: *admin, Status: "active", CreatedAt: now, UpdatedAt: now,
		}
		if err := st.InsertUser(ctx, &u); err != nil {
			return err
		}
		fmt.Printf("created user %s (%s) admin=%v\n", u.Username, u.Email, u.IsAdmin)
		return nil

	case "rm", "remove":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		identifier := fs.String("username", "", "username or email")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *identifier == "" {
			return errors.New("--username <username|email> is required")
		}
		u, err := st.GetUserByIdentifier(ctx, *identifier)
		if err != nil {
			return err
		}
		if err := st.DeleteUser(ctx, u.ID); err != nil {
			return err
		}
		fmt.Printf("removed user %s\n", u.Username)
		return nil
	}
	return fmt.Errorf("unknown user subcommand %q", args[0])
}

// uuid returns a random RFC 4122 v4 UUID (for CLI-created rows).
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// loadConfig reads and validates environment variables.
func loadConfig() (config, error) {
	cfg := config{
		apiKey:           os.Getenv("OPENCODE_API_KEY"),
		tavilyKey:        os.Getenv("TAVILY_API_KEY"),
		braveKey:         os.Getenv("BRAVE_API_KEY"),
		searchProvider:   envOr("MYOGCHAT_SEARCH_PROVIDER", "auto"),
		searchResults:    5,
		searchRegion:     envOr("MYOGCHAT_SEARCH_REGION", "us-en"),
		searchSafeSearch: envOr("MYOGCHAT_SEARCH_SAFESEARCH", "moderate"),
		port:             envOr("MYOGCHAT_PORT", "8787"),
		dbPath:           os.Getenv("MYOGCHAT_DB"),
		retentionDays:    30,
		sandboxEnabled:   sandbox.EnabledFromEnv(),
		sandbox:          sandbox.ConfigFromEnv(),
		// Image OCR (feature 2.5). On by default; MYOGCHAT_OCR=0 disables it
		// entirely (no engine is built, so images are always ErrUnsupported).
		ocrEnabled: envOr("MYOGCHAT_OCR", "1") != "0",
		ocrImage:   envOr("MYOGCHAT_OCR_IMAGE", ocr.DefaultImage),
		ocrLangs:   envOr("MYOGCHAT_OCR_LANGS", ocr.DefaultLangs),
		ocrDocker:  envOr("MYOGCHAT_DOCKER", ocr.DefaultDocker),
		ocrTimeout: ocr.DefaultTimeout,
		// Self-hosted image generation (feature 3.1). Unset by default; the
		// admin can also set the URL/token in Settings → Integrations.
		imagegenURL:   os.Getenv("MYOGCHAT_IMAGEGEN_URL"),
		imagegenToken: os.Getenv("MYOGCHAT_IMAGEGEN_TOKEN"),
	}
	if v := os.Getenv("MYOGCHAT_OCR_TIMEOUT"); v != "" {
		d, err := parseOCRDuration(v)
		if err != nil {
			return config{}, err
		}
		cfg.ocrTimeout = d
	}
	if cfg.dbPath == "" {
		p, err := resolveDBPath()
		if err != nil {
			return config{}, err
		}
		cfg.dbPath = p
	}
	if v := os.Getenv("MYOGCHAT_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return config{}, errors.New("MYOGCHAT_RETENTION_DAYS must be an integer")
		}
		// Contract: retention is clamped to [1, 60].
		if n < 1 {
			n = 1
		}
		if n > 60 {
			n = 60
		}
		cfg.retentionDays = n
	}
	if v := os.Getenv("MYOGCHAT_SEARCH_RESULTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return config{}, errors.New("MYOGCHAT_SEARCH_RESULTS must be an integer")
		}
		// Contract: results per query are clamped to [1, 20].
		if n < 1 {
			n = 1
		}
		if n > 20 {
			n = 20
		}
		cfg.searchResults = n
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseOCRDuration accepts a Go duration ("30s") or a bare integer number of
// seconds ("30") for MYOGCHAT_OCR_TIMEOUT.
func parseOCRDuration(v string) (time.Duration, error) {
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return 0, errors.New("MYOGCHAT_OCR_TIMEOUT must be positive")
		}
		return d, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return 0, errors.New("MYOGCHAT_OCR_TIMEOUT must be positive")
		}
		return time.Duration(n) * time.Second, nil
	}
	return 0, errors.New("MYOGCHAT_OCR_TIMEOUT must be a duration (e.g. 30s) or a number of seconds")
}

func run(cfg config, log *slog.Logger) error {
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	secret, err := ensureSecret(ctx, st, log, sessionSecretKey)
	if err != nil {
		return err
	}
	encKey, err := ensureSecret(ctx, st, log, encryptionKey)
	if err != nil {
		return err
	}
	// Self-registration defaults to open; the admin can disable it in the UI.
	if _, err := st.EnsureSetting(ctx, auth.SettingAllowRegistration, "1"); err != nil {
		return err
	}
	// Sharing the server key with users defaults to on (§2.1).
	if _, err := st.EnsureSetting(ctx, "share_server_key", "1"); err != nil {
		return err
	}
	// The code sandbox (2.6/2.7) is on by default; an admin can turn it off.
	if _, err := st.EnsureSetting(ctx, "sandbox_enabled", "1"); err != nil {
		return err
	}
	// Image OCR defaults to on; the admin can toggle it in Settings (2.5).
	if _, err := st.EnsureSetting(ctx, "ocr_enabled", "1"); err != nil {
		return err
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}

	// The sandbox runner is nil when disabled by env; tools are then never
	// registered even if the DB setting is on.
	var sbx *sandbox.Runner
	if cfg.sandboxEnabled {
		sbx = sandbox.NewRunner(cfg.sandbox)
	}

	// Build the OCR engine only when enabled at startup (MYOGCHAT_OCR). The
	// DB-backed ocr_enabled setting gates it per request on top of this.
	var ocrEngine ocr.Engine
	if cfg.ocrEnabled {
		ocrEngine = ocr.NewDockerTesseract(ocr.Config{
			Image:   cfg.ocrImage,
			Langs:   cfg.ocrLangs,
			Docker:  cfg.ocrDocker,
			Timeout: cfg.ocrTimeout,
		})
	}

	srv := server.New(st, upstream.NewClient(), auth.New(st, secret), static, log, encKey, cfg.apiKey, server.SearchDefaults{
		Provider:   cfg.searchProvider,
		TavilyKey:  cfg.tavilyKey,
		BraveKey:   cfg.braveKey,
		MaxResults: cfg.searchResults,
		Region:     cfg.searchRegion,
		SafeSearch: cfg.searchSafeSearch,
	}, sbx, ocrEngine, cfg.imagegenURL, cfg.imagegenToken)

	go janitor(ctx, st, cfg.retentionDays, log)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE responses are long-lived and streamed.
	}

	users, _ := st.CountUsers(ctx)
	log.Info("myogchat starting",
		"addr", httpSrv.Addr,
		"db", cfg.dbPath,
		"retention_days", cfg.retentionDays,
		"users", users,
		"needs_setup", users == 0,
		"server_key", cfg.apiKey != "",
		"tavily_key", cfg.tavilyKey != "",
		"brave_key", cfg.braveKey != "",
		"search_provider", cfg.searchProvider,
		"search_results", cfg.searchResults,
		"sandbox", cfg.sandboxEnabled,
		"sandbox_docker", cfg.sandbox.Docker,
		"sandbox_network", cfg.sandbox.Network,
		"ocr", cfg.ocrEnabled,
		"imagegen_url", cfg.imagegenURL,
	)

	errc := make(chan error, 1)
	go func() {
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// ensureSecret returns the base64-decoded value of a settings key, generating
// and persisting 32 random bytes on first run. Used for the session-signing
// secret and the per-user-key encryption key.
func ensureSecret(ctx context.Context, st *store.Store, log *slog.Logger, name string) ([]byte, error) {
	v, ok, err := st.GetSetting(ctx, name)
	if err != nil {
		return nil, err
	}
	if !ok || v == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		v = base64.RawURLEncoding.EncodeToString(buf)
		if err := st.SetSetting(ctx, name, v); err != nil {
			return nil, err
		}
		log.Info("generated a new secret", "name", name)
	}
	secret, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, errors.New(name + " is not valid base64")
	}
	return secret, nil
}

// janitor purges conversations older than the retention window on startup and
// then hourly (contract §7). It runs until ctx is canceled.
func janitor(ctx context.Context, st *store.Store, retentionDays int, log *slog.Logger) {
	purge := func() {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
		n, err := st.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			log.Error("retention janitor", "error", err)
			return
		}
		if n > 0 {
			log.Info("retention janitor purged conversations", "count", n, "cutoff", cutoff)
		}
		// Attachments share the retention window; a purged attachment is
		// silently skipped when an old message is regenerated (2.3).
		if a, err := st.DeleteAttachmentsOlderThan(ctx, cutoff); err != nil {
			log.Error("retention janitor", "error", err)
		} else if a > 0 {
			log.Info("retention janitor purged attachments", "count", a, "cutoff", cutoff)
		}
		// Generated images share the retention window too (3.1).
		if img, err := st.DeleteImagesOlderThan(ctx, cutoff); err != nil {
			log.Error("retention janitor", "error", err)
		} else if img > 0 {
			log.Info("retention janitor purged images", "count", img, "cutoff", cutoff)
		}
	}

	purge()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}
