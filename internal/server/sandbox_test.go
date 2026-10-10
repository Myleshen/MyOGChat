package server

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"myogchat/internal/sandbox"
	"myogchat/internal/store"
)

func newSandboxTestServer(t *testing.T, sbx *sandbox.Runner) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{st: st, sbx: sbx, log: slog.Default()}, st
}

// probeRunner returns a runner whose Docker probe succeeds or fails.
func probeRunner(ok bool) *sandbox.Runner {
	return sandbox.NewRunnerWithExec(sandbox.DefaultConfig(), func(_ context.Context, _ string, _ []string, _ []byte) ([]byte, []byte, error) {
		if ok {
			return []byte("29.0.0"), nil, nil
		}
		return nil, []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
	})
}

func TestSandboxToolsRegistered(t *testing.T) {
	srv, _ := newSandboxTestServer(t, probeRunner(true))
	reg := srv.toolRegistry(context.Background())
	if _, ok := reg.Get("run_bash"); !ok {
		t.Error("run_bash should be registered when sandbox is on and Docker is reachable")
	}
	if _, ok := reg.Get("run_python"); !ok {
		t.Error("run_python should be registered")
	}
}

func TestSandboxToolsDisabledBySetting(t *testing.T) {
	srv, st := newSandboxTestServer(t, probeRunner(true))
	if err := st.SetSetting(context.Background(), settingSandboxEnabled, "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	reg := srv.toolRegistry(context.Background())
	if _, ok := reg.Get("run_bash"); ok {
		t.Error("run_bash must not be registered when sandbox_enabled=0")
	}
	if _, ok := reg.Get("run_python"); ok {
		t.Error("run_python must not be registered when sandbox_enabled=0")
	}
	// Non-sandbox tools remain.
	if _, ok := reg.Get("fetch_url"); !ok {
		t.Error("fetch_url should still be registered")
	}
}

func TestSandboxToolsSkippedWhenDockerUnavailable(t *testing.T) {
	srv, _ := newSandboxTestServer(t, probeRunner(false))
	reg := srv.toolRegistry(context.Background())
	if _, ok := reg.Get("run_bash"); ok {
		t.Error("run_bash must not be registered when Docker is unreachable")
	}
}

func TestSandboxToolsSkippedWhenRunnerNil(t *testing.T) {
	srv, _ := newSandboxTestServer(t, nil)
	reg := srv.toolRegistry(context.Background())
	if _, ok := reg.Get("run_bash"); ok {
		t.Error("run_bash must not be registered when the runner is nil (env kill switch)")
	}
}

func TestSandboxToolsDisabledWithTools(t *testing.T) {
	srv, st := newSandboxTestServer(t, probeRunner(true))
	if err := st.SetSetting(context.Background(), settingToolsEnabled, "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	reg := srv.toolRegistry(context.Background())
	if reg.Len() != 0 {
		t.Errorf("registry should be empty when tools_enabled=0, got %d tools", reg.Len())
	}
}

func TestSandboxEnabledDefaultsTrue(t *testing.T) {
	srv, _ := newSandboxTestServer(t, nil)
	if !srv.sandboxEnabled(context.Background()) {
		t.Error("sandboxEnabled should default to true")
	}
}

func TestSandboxAvailableStatus(t *testing.T) {
	srv, _ := newSandboxTestServer(t, probeRunner(true))
	if got := srv.integrationSettings(context.Background())["sandbox_available"]; got != true {
		t.Errorf("sandbox_available = %v, want true", got)
	}
	srv2, _ := newSandboxTestServer(t, probeRunner(false))
	if got := srv2.integrationSettings(context.Background())["sandbox_available"]; got != false {
		t.Errorf("sandbox_available = %v, want false", got)
	}
}
