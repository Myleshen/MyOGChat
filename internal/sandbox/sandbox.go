// Package sandbox runs model-generated code inside a hardened, resource-limited,
// ephemeral Docker container (Phase 2.6 Python / 2.7 Bash).
//
// Each call starts a brand-new container (`docker run --rm`) with a read-only
// root filesystem, a non-root user, all capabilities dropped, a dedicated
// user-defined bridge network (no published ports → no inbound path), strict
// CPU/memory/PID limits and a wall-clock timeout. The code is fed to the
// interpreter over stdin; it is NEVER interpolated into the command line.
//
// The package is deliberately free of server/upstream types so it can be unit
// tested with an injected command executor (no Docker required).
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Language identifiers accepted by Spec.
const (
	LanguageBash   = "bash"
	LanguagePython = "python"
)

// Defaults (all overridable via environment — see ConfigFromEnv).
const (
	DefaultDocker      = "/usr/bin/docker"
	DefaultNetwork     = "myogchat-sbx"
	DefaultBashImage   = "myogchat-sandbox-bash"
	DefaultPythonImage = "myogchat-sandbox-python"
	DefaultMemory      = "256m"
	DefaultCPUs        = "1"
	DefaultPidsLimit   = 128
	DefaultNoFile      = "256:256"
	DefaultTmpSize     = "16m"
	DefaultMaxOutput   = 100_000
	DefaultTimeout     = 30 * time.Second

	// Artifact harvest caps (5.8). A per-file docker --ulimit fsize gives the
	// hard in-container cap; the harvest caps are the second line of defence
	// against storing too much.
	DefaultMaxFileSize   = 10 << 20 // 10 MB
	DefaultMaxTotalSize  = 25 << 20 // 25 MB
	DefaultMaxFiles      = 20
	DefaultFileSizeLimit = "10485760:10485760" // docker --ulimit fsize soft:hard
	DefaultWorkDirName   = "myogchat/sandbox"  // under ~/.local/share

	// Agent mode uses a persistent workspace, so its per-file cap is 2 GiB
	// rather than an ephemeral run's 10 MiB, and the whole user tree is
	// quota'd. Docker's --ulimit parser takes plain bytes only — no unit
	// suffixes like "2g" (it fails the whole `docker run` with exit 125).
	DefaultAgentFileSizeLimit = "2147483648:2147483648" // soft:hard, 2 GiB
	DefaultAgentQuotaBytes    = 5 << 30                 // 5 GiB

	// AgentWorkspaceDirName is the subdirectory of WorkDir that holds
	// persistent agent workspaces (<WorkDir>/agent/<userID>/<convID>).
	AgentWorkspaceDirName = "agent"

	// AgentWorkspaceMaxAge bounds how long an idle conversation workspace may
	// survive before the hourly sweep removes it.
	AgentWorkspaceMaxAge = 7 * 24 * time.Hour

	// workspaceMaxAge bounds how long a crashed run's workspace may survive
	// before the startup/hourly sweep removes it. Real runs are <= Timeout.
	workspaceMaxAge = time.Hour

	// quotaCacheTTL bounds how often the quota tree is walked; a 60-minute
	// agent run would otherwise re-walk gigabytes on every tool call.
	quotaCacheTTL = 60 * time.Second

	// LabelKey/Value tag every sandbox container so leaked containers can be
	// cleaned up with `docker rm -f $(docker ps -aq --filter label=...)`.
	LabelKey   = "myogchat-sandbox"
	LabelValue = "1"

	// containerUser is nobody:nogroup — the container never runs as root. It is
	// the fallback only when the service itself runs as root.
	containerUser = "65534:65534"

	// probeTTL bounds how often the Docker reachability probe runs.
	probeTTL = 30 * time.Second

	// auxiliaryTimeout bounds the short helper commands (network create,
	// probe, cleanup) so they cannot hang the request.
	auxiliaryTimeout = 10 * time.Second

	// truncMarker is appended when captured output is clipped.
	truncMarker = "\n…[output truncated]"
)

// Sentinel errors, returned by Run and matched with errors.Is. Container
// scripts that exit non-zero are NOT errors — they come back as a Result.
var (
	// ErrDockerNotFound means the docker executable could not be located.
	ErrDockerNotFound = errors.New("docker executable not found (set MYOGCHAT_DOCKER)")
	// ErrDockerUnavailable means the docker daemon is not reachable.
	ErrDockerUnavailable = errors.New("docker daemon is not reachable")
	// ErrImageMissing means the sandbox image is not present locally.
	ErrImageMissing = errors.New("sandbox image is not available locally")
	// ErrNetworkMissing means the sandbox network could not be created.
	ErrNetworkMissing = errors.New("sandbox docker network is unavailable")
	// ErrTimeout means the container exceeded the per-call wall-clock limit and
	// was force-removed.
	ErrTimeout = errors.New("sandbox execution timed out")
	// ErrQuotaExceeded means the agent workspace tree is at or over its disk
	// quota, so the call was refused before anything ran.
	ErrQuotaExceeded = errors.New("agent workspace quota exceeded")
)

// Config holds the hardened container parameters.
type Config struct {
	Docker      string        // path to the docker CLI
	Network     string        // dedicated user-defined bridge network
	BashImage   string        // image for run_bash
	PythonImage string        // image for run_python
	Timeout     time.Duration // per-call wall-clock limit
	Memory      string        // docker --memory (and --memory-swap) value
	CPUs        string        // docker --cpus value
	PidsLimit   int           // docker --pids-limit value
	NoFile      string        // docker --ulimit nofile= value (soft:hard)
	RunAsUser   string        // docker --user uid:gid (host user, so writes stay deletable)
	TmpSize     string        // size of the /tmp tmpfs
	MaxOutput   int           // max characters of stdout+stderr returned
	// Workspace + artifact harvest (5.8).
	WorkDir       string // host directory root for per-run workspaces (never /tmp)
	FileSizeLimit string // docker --ulimit fsize= value for ephemeral runs (soft:hard)
	MaxFileSize   int64  // per-file harvest cap (bytes)
	MaxTotalSize  int64  // total harvest cap (bytes)
	MaxFiles      int    // max harvested files per run
	// Agent mode: persistent workspaces are much larger than ephemeral ones.
	AgentFileSizeLimit string // docker --ulimit fsize= value for persistent runs
	AgentQuotaBytes    int64  // default per-user workspace quota (0 = no check)
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Docker:      resolveDocker(""),
		Network:     DefaultNetwork,
		BashImage:   DefaultBashImage,
		PythonImage: DefaultPythonImage,
		Timeout:     DefaultTimeout,
		Memory:      DefaultMemory,
		CPUs:        DefaultCPUs,
		PidsLimit:   DefaultPidsLimit,
		NoFile:      DefaultNoFile,
		RunAsUser:   defaultRunAsUser(os.Geteuid(), os.Getegid()),
		TmpSize:     DefaultTmpSize,
		MaxOutput:   DefaultMaxOutput,

		WorkDir:       resolveWorkDir(""),
		FileSizeLimit: DefaultFileSizeLimit,
		MaxFileSize:   DefaultMaxFileSize,
		MaxTotalSize:  DefaultMaxTotalSize,
		MaxFiles:      DefaultMaxFiles,

		AgentFileSizeLimit: DefaultAgentFileSizeLimit,
		AgentQuotaBytes:    DefaultAgentQuotaBytes,
	}
}

// defaultRunAsUser is the uid:gid the sandbox container runs as: the service's
// own user, so everything it writes can be removed by the service later (a
// nobody-owned 0755 directory is not removable by uid 1000). A root service
// falls back to nobody — the container must never run as uid 0.
func defaultRunAsUser(euid, egid int) string {
	if euid <= 0 {
		return containerUser
	}
	return fmt.Sprintf("%d:%d", euid, egid)
}

// ConfigFromEnv overlays environment variables on DefaultConfig:
//
//	MYOGCHAT_DOCKER               docker CLI path (default /usr/bin/docker, else LookPath)
//	MYOGCHAT_SANDBOX_NETWORK      network name (default myogchat-sbx)
//	MYOGCHAT_SANDBOX_IMAGE_BASH   bash image (default myogchat-sandbox-bash)
//	MYOGCHAT_SANDBOX_IMAGE_PYTHON python image (default myogchat-sandbox-python)
//	MYOGCHAT_SANDBOX_MEMORY       memory limit (default 256m)
//	MYOGCHAT_SANDBOX_CPUS         CPU limit (default 1)
//	MYOGCHAT_SANDBOX_TIMEOUT      seconds (default 30)
//	MYOGCHAT_SANDBOX_MAX_OUTPUT   characters (default 100000)
//	MYOGCHAT_SANDBOX_WORKDIR      host root for per-run workspaces (default
//	                              ~/.local/share/myogchat/sandbox)
//	MYOGCHAT_AGENT_QUOTA_BYTES    per-user agent workspace quota in bytes
//	                              (default 5368709120, i.e. 5 GiB; 0 disables)
func ConfigFromEnv() Config {
	c := DefaultConfig()
	c.Docker = resolveDocker(os.Getenv("MYOGCHAT_DOCKER"))
	if v := os.Getenv("MYOGCHAT_SANDBOX_NETWORK"); v != "" {
		c.Network = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_IMAGE_BASH"); v != "" {
		c.BashImage = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_IMAGE_PYTHON"); v != "" {
		c.PythonImage = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_MEMORY"); v != "" {
		c.Memory = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_CPUS"); v != "" {
		c.CPUs = v
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Timeout = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_MAX_OUTPUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxOutput = n
		}
	}
	if v := os.Getenv("MYOGCHAT_SANDBOX_WORKDIR"); v != "" {
		c.WorkDir = v
	}
	if v := os.Getenv("MYOGCHAT_AGENT_QUOTA_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.AgentQuotaBytes = n
		}
	}
	return c
}

// EnabledFromEnv reports whether the sandbox is enabled at all via
// MYOGCHAT_SANDBOX (a hard kill switch, default on). Accepted false values:
// 0, false, off, no (case-insensitive).
func EnabledFromEnv() bool {
	v := strings.TrimSpace(os.Getenv("MYOGCHAT_SANDBOX"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// resolveDocker picks the docker CLI: an explicit value wins; otherwise
// /usr/bin/docker when present, falling back to $PATH.
func resolveDocker(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat(DefaultDocker); err == nil {
		return DefaultDocker
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	return DefaultDocker
}

// resolveWorkDir picks the host root for per-run sandbox workspaces. An explicit
// value wins; otherwise ~/.local/share/myogchat/sandbox. /tmp is deliberately
// avoided: the systemd unit sets PrivateTmp=true, so a container in a separate
// mount namespace could not see a host /tmp path anyway.
func resolveWorkDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Degenerate fallback (no home dir); the per-run dir is still created
		// with a random name and removed on completion.
		return filepath.Join(os.TempDir(), "myogchat-sandbox")
	}
	return filepath.Join(home, ".local", "share", filepath.FromSlash(DefaultWorkDirName))
}

// Spec is one execution request.
type Spec struct {
	Language string // LanguageBash or LanguagePython
	Code     string // fed to the interpreter on stdin
	// Workspace is an absolute host directory bind-mounted at /work and kept
	// after the call (agent mode). Empty means a fresh per-run directory that is
	// removed afterwards (today's ephemeral behaviour).
	Workspace string
	// QuotaDir is the user-level directory measured against QuotaBytes before a
	// call runs. Empty means no quota check.
	QuotaDir string
	// QuotaBytes is the disk limit for QuotaDir; 0 disables the check.
	QuotaBytes int64
	// Timeout overrides Config.Timeout for this call only; 0 uses the config.
	Timeout time.Duration
}

// Result is the outcome of a sandboxed run.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
	// Artifacts are the regular files the script left in /work, harvested after
	// the run (name sanitized, MIME sniffed, caps enforced). Empty when none.
	Artifacts []Artifact
}

// Artifact is one file produced by a sandbox run: a sanitized base name, a
// sniffed MIME type and the raw bytes. The bytes are stored opaque and served
// only as a download (5.8).
type Artifact struct {
	Name string
	MIME string
	Data []byte
}

// ExecFunc runs one command and returns captured stdout/stderr. It is injectable
// so the runner is unit-testable without Docker.
type ExecFunc func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr []byte, err error)

// Runner executes code in sandbox containers.
type Runner struct {
	cfg  Config
	exec ExecFunc

	mu       sync.Mutex
	probeOK  bool
	probeAt  time.Time
	netReady bool

	quotaMu sync.Mutex
	quota   map[string]quotaEntry // QuotaDir -> last measurement
}

// quotaEntry is one cached DirSize result.
type quotaEntry struct {
	size int64
	at   time.Time
}

// NewRunner returns a Runner using the real os/exec executor.
func NewRunner(cfg Config) *Runner {
	return NewRunnerWithExec(cfg, osExec(captureLimit(cfg.MaxOutput)))
}

// NewRunnerWithExec returns a Runner using a custom executor (tests).
func NewRunnerWithExec(cfg Config, exec ExecFunc) *Runner {
	return &Runner{cfg: sanitizeRunAsUser(sanitizeFileSizeLimits(cfg)), exec: exec}
}

// fileSizeLimitPattern is a docker-parseable --ulimit fsize value: plain byte
// counts as soft:hard. Docker's ulimit parser accepts integers only, so "2g:2g"
// fails every `docker run` with exit 125.
var fileSizeLimitPattern = regexp.MustCompile(`^\d+:\d+$`)

// sanitizeFileSizeLimits replaces an unusable fsize limit with its default and
// warns, so a bad value degrades to a working sandbox instead of breaking every
// sandboxed call.
func sanitizeFileSizeLimits(cfg Config) Config {
	if !fileSizeLimitPattern.MatchString(cfg.FileSizeLimit) {
		slog.Default().Warn("sandbox: invalid fsize limit, using default",
			"value", cfg.FileSizeLimit, "default", DefaultFileSizeLimit)
		cfg.FileSizeLimit = DefaultFileSizeLimit
	}
	if !fileSizeLimitPattern.MatchString(cfg.AgentFileSizeLimit) {
		slog.Default().Warn("sandbox: invalid agent fsize limit, using default",
			"value", cfg.AgentFileSizeLimit, "default", DefaultAgentFileSizeLimit)
		cfg.AgentFileSizeLimit = DefaultAgentFileSizeLimit
	}
	return cfg
}

// userPattern is a docker --user value: numeric uid:gid, never a name (the
// image may not have an /etc/passwd entry for the host user).
var userPattern = regexp.MustCompile(`^\d+:\d+$`)

// sanitizeRunAsUser replaces an unusable --user value (empty, malformed, or a
// root uid) with the safe default, so a misconfiguration cannot run the
// container as root or as a uid the service cannot clean up after.
func sanitizeRunAsUser(cfg Config) Config {
	uid, _, _ := strings.Cut(cfg.RunAsUser, ":")
	n, err := strconv.Atoi(uid)
	if err != nil || n <= 0 || !userPattern.MatchString(cfg.RunAsUser) {
		safe := defaultRunAsUser(os.Geteuid(), os.Getegid())
		slog.Default().Warn("sandbox: invalid run-as user, using default",
			"value", cfg.RunAsUser, "default", safe)
		cfg.RunAsUser = safe
	}
	return cfg
}

// Config returns the runner's configuration.
func (r *Runner) Config() Config { return r.cfg }

// Docker returns the docker CLI path in use (for status display).
func (r *Runner) Docker() string { return r.cfg.Docker }

// captureLimit is the raw per-stream capture cap: four times the returned
// budget so truncate still has something to clip to MaxOutput.
func captureLimit(maxOutput int) int {
	if maxOutput <= 0 {
		return 0 // unbounded (MaxOutput disables truncation)
	}
	return maxOutput * 4
}

// limitedBuf is a bytes.Buffer that stops storing after limit bytes, so a
// chatty script cannot exhaust the Pi's RAM before truncation. Writes past the
// limit are discarded and still report full length, so the child never blocks
// on a full pipe. limit <= 0 is unbounded.
type limitedBuf struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedBuf) Write(p []byte) (int, error) {
	n := len(p)
	if w.limit > 0 {
		if room := w.limit - w.buf.Len(); room < n {
			if room < 0 {
				room = 0
			}
			p = p[:room]
			w.truncated = true
		}
	}
	_, _ = w.buf.Write(p)
	return n, nil
}

// osExec returns the default ExecFunc: exec.CommandContext with piped
// stdin/stdout, capturing at most maxBytes per stream.
func osExec(maxBytes int) ExecFunc {
	return func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		out := &limitedBuf{limit: maxBytes}
		errb := &limitedBuf{limit: maxBytes}
		cmd.Stdout = out
		cmd.Stderr = errb
		err := cmd.Run()
		return out.buf.Bytes(), errb.buf.Bytes(), err
	}
}

// Available reports whether the Docker daemon is reachable. The result is cached
// for probeTTL so per-request registration does not shell out every time.
func (r *Runner) Available(ctx context.Context) bool {
	r.mu.Lock()
	if !r.probeAt.IsZero() && time.Since(r.probeAt) < probeTTL {
		ok := r.probeOK
		r.mu.Unlock()
		return ok
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
	defer cancel()
	_, _, err := r.exec(ctx, r.cfg.Docker, []string{"info", "--format", "{{.ServerVersion}}"}, nil)
	ok := err == nil

	r.mu.Lock()
	r.probeOK = ok
	r.probeAt = time.Now()
	r.mu.Unlock()
	return ok
}

// Run executes spec and returns its result. A non-zero exit code is reported in
// Result (not as an error); failures to launch the container return a sentinel
// error (ErrImageMissing, ErrNetworkMissing, ErrDockerUnavailable, …).
func (r *Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	image, command, err := r.imageAndCommand(spec)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(spec.Code) == "" {
		return Result{}, errors.New("code is required")
	}
	if spec.QuotaBytes > 0 && spec.QuotaDir != "" {
		if err := r.checkQuota(spec); err != nil {
			return Result{}, err
		}
	}
	if err := r.ensureNetwork(ctx); err != nil {
		return Result{}, err
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = r.cfg.Timeout
	}
	fileSizeLimit := r.cfg.FileSizeLimit
	if spec.Workspace != "" {
		fileSizeLimit = r.cfg.AgentFileSizeLimit
	}

	name := r.containerName()
	var (
		runDir string
		since  time.Time // persistent workspaces harvest only this call's files
	)
	if spec.Workspace != "" {
		runDir = spec.Workspace
		// Keep the workspace root 0700 like the ephemeral root, so unrelated
		// local users cannot enumerate persisted conversation files.
		if err := os.MkdirAll(r.cfg.WorkDir, 0o700); err != nil {
			return Result{}, fmt.Errorf("create sandbox workdir: %w", err)
		}
		if err := preparePersistentWorkspace(runDir); err != nil {
			return Result{}, err
		}
	} else {
		var cleanupDir func()
		var err error
		runDir, cleanupDir, err = r.prepareWorkspace(name)
		if err != nil {
			return Result{}, err
		}
		defer cleanupDir()
	}
	args := r.runArgs(image, command, name, runDir, fileSizeLimit)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	if spec.Workspace != "" {
		since = start.Add(-time.Second)
	}
	stdout, stderr, execErr := r.exec(runCtx, r.cfg.Docker, args, []byte(spec.Code))
	res := Result{
		Stdout:   string(stdout),
		Stderr:   string(stderr),
		Duration: time.Since(start),
	}
	// Harvest the workspace before the container is force-removed and before the
	// deferred RemoveAll. The harvest never follows symlinks and enforces the
	// per-file/count/total caps (5.8).
	res.Artifacts = harvestSince(runDir, r.cfg, since)
	res.Truncated = r.truncate(&res)

	// Timeout: the container must not leak, so force-remove it by name.
	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		r.cleanup(name)
		return res, fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	// Caller cancelled (e.g. client disconnect): also remove the container.
	if ctx.Err() != nil {
		r.cleanup(name)
		return res, ctx.Err()
	}
	if execErr != nil {
		code := exitCode(execErr)
		// Exit code 125 is docker run's own failure; known daemon/image/network
		// messages are docker-level regardless of the code.
		if code == 125 || dockerLevelFailure(res.Stderr) || code < 0 {
			return res, classifyRunError(execErr, res.Stderr)
		}
		// The container ran and its command exited non-zero — a normal result.
		res.ExitCode = code
		return res, nil
	}
	res.ExitCode = 0
	return res, nil
}

// imageAndCommand maps a Spec to its image and container command. The command
// reads the program from stdin (`bash -s` / `python -`); the code itself is
// never placed on the command line.
func (r *Runner) imageAndCommand(spec Spec) (string, []string, error) {
	switch spec.Language {
	case LanguageBash:
		return r.cfg.BashImage, []string{"bash", "-s"}, nil
	case LanguagePython:
		return r.cfg.PythonImage, []string{"python", "-"}, nil
	default:
		return "", nil, fmt.Errorf("unknown sandbox language %q", spec.Language)
	}
}

// prepareWorkspace creates a fresh per-run host directory that is bind-mounted
// rw into the container as /work. The parent root is 0700 (only the service
// user can traverse it); the per-run dir is made group/other-writable so the
// container user (host uid by default, nobody when the service is root) can
// write into it. The returned cleanup removes the dir; callers defer it.
func (r *Runner) prepareWorkspace(name string) (string, func(), error) {
	root := r.cfg.WorkDir
	if root == "" {
		return "", func() {}, errors.New("sandbox workdir is not configured")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create sandbox workdir: %w", err)
	}
	runDir := filepath.Join(root, name)
	if err := os.Mkdir(runDir, 0o777); err != nil {
		return "", func() {}, fmt.Errorf("create sandbox workspace: %w", err)
	}
	// Mkdir is filtered by umask, so make the mode explicit: the container user
	// (host uid by default) is not necessarily the service user and must be
	// able to write.
	if err := os.Chmod(runDir, 0o777); err != nil {
		_ = os.RemoveAll(runDir)
		return "", func() {}, fmt.Errorf("chmod sandbox workspace: %w", err)
	}
	return runDir, func() { _ = os.RemoveAll(runDir) }, nil
}

// preparePersistentWorkspace makes an agent workspace writable by the container
// user and touches its mtime, so the idle sweep sees recent use. It is never
// removed here: the directory belongs to the conversation. Intermediate
// directories are traverse-only (0711), keeping the 0700 WorkDir invariant.
func preparePersistentWorkspace(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("sandbox workspace %q is not an absolute path", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o711); err != nil {
		return fmt.Errorf("create agent workspace: %w", err)
	}
	if err := os.Mkdir(dir, 0o777); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create agent workspace: %w", err)
	}
	// Mkdir is filtered by umask, so make the mode explicit: the container user
	// (host uid by default) is not necessarily the service user and must be
	// able to write.
	if err := os.Chmod(dir, 0o777); err != nil {
		return fmt.Errorf("chmod agent workspace: %w", err)
	}
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		return fmt.Errorf("touch agent workspace: %w", err)
	}
	return nil
}

// SweepLeftovers removes per-run workspaces older than workspaceMaxAge. It
// covers crash leftovers (a SIGKILL mid-run skips the deferred RemoveAll) and is
// safe to call at startup and hourly from the retention janitor. Persistent
// agent workspaces under agent/ are skipped — they are swept by
// SweepAgentWorkspaces with a much longer idle window.
func (r *Runner) SweepLeftovers() {
	root := r.cfg.WorkDir
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-workspaceMaxAge)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == AgentWorkspaceDirName {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// SweepAgentWorkspaces removes conversation directories under
// <WorkDir>/agent/ whose mtime is older than idle, then any user directory left
// empty. It returns how many conversation workspaces were removed.
func (r *Runner) SweepAgentWorkspaces(idle time.Duration) (int, error) {
	root := filepath.Join(r.cfg.WorkDir, AgentWorkspaceDirName)
	users, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read agent workspace root: %w", err)
	}
	cutoff := time.Now().Add(-idle)
	removed := 0
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		userDir := filepath.Join(root, u.Name())
		entries, err := os.ReadDir(userDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.IsDir() || !info.ModTime().Before(cutoff) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(userDir, e.Name())); err != nil {
				return removed, fmt.Errorf("remove agent workspace: %w", err)
			}
			removed++
		}
		if rest, err := os.ReadDir(userDir); err == nil && len(rest) == 0 {
			_ = os.Remove(userDir)
		}
	}
	return removed, nil
}

// DirSize returns the total size of the regular files under path. Symlinks are
// not followed and unreadable entries are skipped (a quota undercount is safer
// than failing every agent call). A missing path is size 0.
func DirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// FreeBytes returns the bytes available to unprivileged users on the filesystem
// holding path.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// checkQuota refuses a call when QuotaDir is at or over QuotaBytes. The size is
// cached for quotaCacheTTL so a long agent run does not re-walk the tree on
// every call.
func (r *Runner) checkQuota(spec Spec) error {
	size, err := r.quotaSize(spec.QuotaDir)
	if err != nil {
		return fmt.Errorf("measure agent workspace: %w", err)
	}
	if size < spec.QuotaBytes {
		return nil
	}
	return fmt.Errorf("%w: workspace is %s of its %s limit — delete files in /work or remove an old agent workspace",
		ErrQuotaExceeded, formatBytes(size), formatBytes(spec.QuotaBytes))
}

// quotaSize returns the cached size of dir, measuring it again when the cache
// entry is older than quotaCacheTTL.
func (r *Runner) quotaSize(dir string) (int64, error) {
	r.quotaMu.Lock()
	if e, ok := r.quota[dir]; ok && time.Since(e.at) < quotaCacheTTL {
		r.quotaMu.Unlock()
		return e.size, nil
	}
	r.quotaMu.Unlock()

	size, err := DirSize(dir)
	if err != nil {
		return 0, err
	}
	r.quotaMu.Lock()
	if r.quota == nil {
		r.quota = map[string]quotaEntry{}
	}
	r.quota[dir] = quotaEntry{size: size, at: time.Now()}
	r.quotaMu.Unlock()
	return size, nil
}

// formatBytes renders a byte count for a model-facing message.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// runArgs builds the full hardened `docker run` argv. fileSizeLimit is the
// effective --ulimit fsize value for this call (ephemeral vs agent).
func (r *Runner) runArgs(image string, command []string, name, workDir, fileSizeLimit string) []string {
	args := []string{
		"run",
		"--rm",
		"--name", name,
		"--label", LabelKey + "=" + LabelValue,
		"--network", r.cfg.Network,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", r.cfg.RunAsUser,
		"--read-only",
		// /work is a host bind mount so produced files survive the container and
		// can be harvested; /tmp stays an ephemeral tmpfs.
		"-v", workDir + ":/work:rw",
		"--tmpfs", "/tmp:rw,size=" + r.cfg.TmpSize,
		"--workdir", "/work",
		"--memory", r.cfg.Memory,
		"--memory-swap", r.cfg.Memory,
		"--cpus", r.cfg.CPUs,
		"--pids-limit", strconv.Itoa(r.cfg.PidsLimit),
		"--ulimit", "nofile=" + r.cfg.NoFile,
		// A hard per-file size cap so one runaway write cannot exhaust the disk.
		"--ulimit", "fsize=" + fileSizeLimit,
		"--stop-timeout", "2",
		"--pull", "never",
		"-e", "HOME=/work",
		"-e", "TMPDIR=/tmp",
		"-i",
		image,
	}
	return append(args, command...)
}

// ensureNetwork makes sure the dedicated bridge network exists, creating it on
// first use. The result is cached for the runner's lifetime.
func (r *Runner) ensureNetwork(ctx context.Context) error {
	r.mu.Lock()
	if r.netReady {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	inspectCtx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
	_, _, err := r.exec(inspectCtx, r.cfg.Docker, []string{"network", "inspect", r.cfg.Network}, nil)
	cancel()
	if err == nil {
		r.mu.Lock()
		r.netReady = true
		r.mu.Unlock()
		return nil
	}

	createCtx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
	_, stderr, createErr := r.exec(createCtx, r.cfg.Docker, []string{"network", "create", r.cfg.Network}, nil)
	cancel()
	if createErr != nil {
		// A concurrent creator may have won the race; re-check before failing.
		reCtx, cancel := context.WithTimeout(ctx, auxiliaryTimeout)
		_, _, reErr := r.exec(reCtx, r.cfg.Docker, []string{"network", "inspect", r.cfg.Network}, nil)
		cancel()
		if reErr != nil {
			return fmt.Errorf("%w: %s", ErrNetworkMissing, firstLine(string(stderr)))
		}
	}
	r.mu.Lock()
	r.netReady = true
	r.mu.Unlock()
	return nil
}

// cleanup force-removes a container by name so a killed `docker run` client
// cannot leak it. It uses a fresh context: the caller's is already cancelled.
func (r *Runner) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), auxiliaryTimeout)
	defer cancel()
	_, _, _ = r.exec(ctx, r.cfg.Docker, []string{"rm", "-f", name}, nil)
}

// truncate clips stdout+stderr to MaxOutput characters, stdout first. It
// reports whether anything was dropped.
func (r *Runner) truncate(res *Result) bool {
	limit := r.cfg.MaxOutput
	if limit <= 0 {
		return false
	}
	stdout := []rune(res.Stdout)
	if len(stdout) >= limit {
		res.Stdout = string(stdout[:limit]) + truncMarker
		res.Stderr = ""
		return true
	}
	stderr := []rune(res.Stderr)
	if remaining := limit - len(stdout); len(stderr) > remaining {
		res.Stderr = string(stderr[:remaining]) + truncMarker
		return true
	}
	return false
}

// containerName returns a unique name for one run.
func (r *Runner) containerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("myogchat-sbx-%d", time.Now().UnixNano())
	}
	return "myogchat-sbx-" + hex.EncodeToString(b[:])
}

// exitCode extracts a process exit code from an error, or -1.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// dockerLevelFailure reports whether stderr carries a Docker-level (not script)
// failure message. Markers are specific so an ordinary script's stderr cannot
// be misclassified.
func dockerLevelFailure(stderr string) bool {
	low := strings.ToLower(stderr)
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"permission denied while trying to connect",
		"unable to find image",
		"no such image",
		"pull access denied",
		"manifest unknown",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return strings.Contains(low, "network") && strings.Contains(low, "not found")
}

// classifyRunError maps a Docker launch failure to a clear, actionable error.
func classifyRunError(execErr error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "cannot connect to the docker daemon"),
		strings.Contains(low, "permission denied while trying to connect"):
		return fmt.Errorf("%w: %s", ErrDockerUnavailable, firstLine(stderr))
	case strings.Contains(low, "unable to find image"),
		strings.Contains(low, "no such image"),
		strings.Contains(low, "pull access denied"),
		strings.Contains(low, "manifest unknown"):
		return fmt.Errorf("%w: run deploy/sandbox-setup.sh to pull the images (%s)", ErrImageMissing, firstLine(stderr))
	case strings.Contains(low, "network") && strings.Contains(low, "not found"):
		return fmt.Errorf("%w: run deploy/sandbox-setup.sh to create it", ErrNetworkMissing)
	}
	if errors.Is(execErr, exec.ErrNotFound) || errors.Is(execErr, fs.ErrNotExist) {
		return ErrDockerNotFound
	}
	return fmt.Errorf("docker run failed: %w", execErr)
}

// firstLine returns the first non-empty line of s, trimmed and capped.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
