// Command myogchat serves the MyOGChat single-page app and its API from one
// binary: embedded frontend, SQLite storage, and an OpenCode Go upstream proxy.
package main

import (
	"context"
	"embed"
	"errors"
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
	"myogchat/internal/server"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// all:static embeds the whole frontend directory, including files whose names
// begin with "_" or "." (embed's default would skip those).
//
//go:embed all:static
var staticFS embed.FS

// config is the resolved runtime configuration (DESIGN.md).
type config struct {
	apiKey        string
	authToken     string
	port          string
	dbPath        string
	retentionDays int
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

// loadConfig reads and validates environment variables, failing fast when a
// required secret is missing.
func loadConfig() (config, error) {
	cfg := config{
		apiKey:        os.Getenv("OPENCODE_API_KEY"),
		authToken:     os.Getenv("MYOGCHAT_AUTH_TOKEN"),
		port:          envOr("MYOGCHAT_PORT", "8787"),
		dbPath:        os.Getenv("MYOGCHAT_DB"),
		retentionDays: 30,
	}
	if cfg.apiKey == "" {
		return config{}, errors.New("OPENCODE_API_KEY is required")
	}
	if cfg.authToken == "" {
		return config{}, errors.New("MYOGCHAT_AUTH_TOKEN is required")
	}
	if cfg.dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return config{}, err
		}
		cfg.dbPath = filepath.Join(home, ".local", "share", "myogchat", "chat.db")
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
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(cfg config, log *slog.Logger) error {
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}

	srv := server.New(st, upstream.NewClient(cfg.apiKey), auth.New(cfg.authToken), static, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go janitor(ctx, st, cfg.retentionDays, log)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE responses are long-lived and streamed.
	}

	log.Info("myogchat starting",
		"addr", httpSrv.Addr,
		"db", cfg.dbPath,
		"retention_days", cfg.retentionDays,
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
