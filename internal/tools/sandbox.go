package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"myogchat/internal/sandbox"
)

// SandboxRunner executes code in an ephemeral sandbox. It is satisfied by
// *sandbox.Runner and by test fakes.
type SandboxRunner interface {
	Run(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error)
}

// ArtifactCollector receives each file a sandbox run produced (5.8). The server
// supplies one to store the bytes and attach a download chip to the reply.
type ArtifactCollector func(sandbox.Artifact)

// Workspace binds a tool to a persistent per-conversation workspace (agent
// mode). Path is the absolute host directory mounted at /work; the runner
// creates it on first use and keeps it between calls.
type Workspace struct {
	Path       string
	QuotaDir   string        // user-level directory measured for the quota
	QuotaBytes int64         // 0 = no quota check
	Timeout    time.Duration // per-call limit; 0 = runner default
}

// RunBashTool runs a Bash script in the sandbox (Phase 2.7).
type RunBashTool struct {
	r       SandboxRunner
	collect ArtifactCollector
	ws      *Workspace // nil = ephemeral per-call workspace
}

// NewRunBashTool returns a run_bash tool backed by r.
func NewRunBashTool(r SandboxRunner) *RunBashTool { return &RunBashTool{r: r} }

// NewRunBashToolWithCollect returns a run_bash tool that reports produced files
// through collect (5.8). A nil collect behaves like NewRunBashTool.
func NewRunBashToolWithCollect(r SandboxRunner, collect ArtifactCollector) *RunBashTool {
	return &RunBashTool{r: r, collect: collect}
}

// NewRunBashToolForWorkspace returns a run_bash tool whose /work is a persistent
// agent workspace shared by every call in the conversation.
func NewRunBashToolForWorkspace(r SandboxRunner, collect ArtifactCollector, ws Workspace) *RunBashTool {
	return &RunBashTool{r: r, collect: collect, ws: &ws}
}

func (t *RunBashTool) Name() string { return "run_bash" }

func (t *RunBashTool) Description() string {
	if t.ws != nil {
		return workspaceBashDescription(t.ws)
	}
	return ephemeralBashDescription
}

func (t *RunBashTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "The Bash script to run (executed with `bash -s`).",
			},
		},
		"required": []string{"code"},
	})
}

func (t *RunBashTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return runSandboxTool(ctx, t.r, sandbox.LanguageBash, args, t.collect, t.ws)
}

// RunPythonTool runs a Python script in the sandbox (Phase 2.6).
type RunPythonTool struct {
	r       SandboxRunner
	collect ArtifactCollector
	ws      *Workspace // nil = ephemeral per-call workspace
}

// NewRunPythonTool returns a run_python tool backed by r.
func NewRunPythonTool(r SandboxRunner) *RunPythonTool { return &RunPythonTool{r: r} }

// NewRunPythonToolWithCollect returns a run_python tool that reports produced
// files through collect (5.8). A nil collect behaves like NewRunPythonTool.
func NewRunPythonToolWithCollect(r SandboxRunner, collect ArtifactCollector) *RunPythonTool {
	return &RunPythonTool{r: r, collect: collect}
}

// NewRunPythonToolForWorkspace returns a run_python tool whose /work is a
// persistent agent workspace shared by every call in the conversation.
func NewRunPythonToolForWorkspace(r SandboxRunner, collect ArtifactCollector, ws Workspace) *RunPythonTool {
	return &RunPythonTool{r: r, collect: collect, ws: &ws}
}

func (t *RunPythonTool) Name() string { return "run_python" }

func (t *RunPythonTool) Description() string {
	if t.ws != nil {
		return workspacePythonDescription(t.ws)
	}
	return ephemeralPythonDescription
}

func (t *RunPythonTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "The Python script to run (executed with `python -`).",
			},
		},
		"required": []string{"code"},
	})
}

func (t *RunPythonTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return runSandboxTool(ctx, t.r, sandbox.LanguagePython, args, t.collect, t.ws)
}

// toolbelt is the shared CLI package list; both sandbox images install it.
const toolbelt = "Pre-installed tools: git, curl, wget, " +
	"openssl, ripgrep (rg), jq, tree, file, less, bc, zip/unzip, xz/bzip2/zstd, " +
	"pdftotext (poppler-utils), sqlite3, ImageMagick (magick/convert) and make."

const ephemeralBashDescription = "Run a Bash script (bash 5.3) inside a fresh, ephemeral, resource-limited " +
	"Linux sandbox container and return its stdout and stderr. The filesystem does " +
	"NOT persist: every call starts a brand-new container, so files written in one " +
	"call are gone by the next. The sandbox has outbound network access (curl, wget " +
	"and git clone work), but is otherwise locked down: no root, read-only root " +
	"filesystem, 1 CPU, 256 MB RAM and a 30-second limit — nothing can be installed " +
	"at run time (no apt-get, apk or pip). " + toolbelt + " Write files " +
	"under the working directory /work: regular files left there when the script " +
	"finishes are delivered to the user as downloadable attachments (up to 20 files, " +
	"10 MB each). Pass the script in `code`; it is piped to `bash -s` on stdin. " +
	"Prefer non-interactive, self-contained scripts that only use the pre-installed " +
	"tools."

const ephemeralPythonDescription = "Run a Python 3.14 script inside a fresh, ephemeral, resource-limited Linux " +
	"sandbox container and return its stdout and stderr. The filesystem does NOT " +
	"persist: every call starts a brand-new container, so files written in one call " +
	"are gone by the next. The sandbox has outbound network access (urllib works), " +
	"but is otherwise locked down: no root, read-only root filesystem, 1 CPU, 256 MB " +
	"RAM and a 30-second limit — nothing can be installed at run time (no pip, " +
	"apt-get or apk). Write files under the working directory /work: regular files " +
	"left there when the script finishes are delivered to the user as downloadable " +
	"attachments (up to 20 files, 10 MB each; the image ships reportlab, openpyxl, " +
	"python-docx, Pillow and matplotlib). The CLI toolbelt is available too: git, " +
	"curl, wget, openssl, ripgrep (rg), jq, tree, file, less, bc, zip/unzip, " +
	"xz/bzip2/zstd, pdftotext (poppler-utils), sqlite3, ImageMagick (magick/convert) " +
	"and make. Pass the script in `code`; it is piped to `python -` on stdin. Use only " +
	"the Python standard library and the pre-installed packages."

func workspaceBashDescription(ws *Workspace) string {
	return "Run a Bash script (bash 5.3) inside a fresh, resource-limited Linux sandbox " +
		"container and return its stdout and stderr. Files under /work persist between " +
		"calls in this conversation for the duration of the job: a file written in one " +
		"call is still there in the next, so build on earlier work instead of redoing it. " +
		"Every call still starts a brand-new container, so running processes do NOT " +
		"persist — restart a server or re-export any environment you need. The sandbox " +
		"has outbound network access (curl, wget and git clone work), but is otherwise " +
		"locked down: no root, read-only root filesystem, 1 CPU, 256 MB RAM and a " +
		limitText(ws.Timeout) + " limit per call — nothing can be installed at run time (no " +
		"apt-get, apk or pip). " + toolbelt + " Write files under the working directory " +
		"/work: regular files created or modified by this call are delivered to the " +
		"user as downloadable attachments (up to 20 files, 10 MB each). " +
		quotaNote(ws) + "Pass the script in `code`; it is piped to `bash -s` on stdin."
}

func workspacePythonDescription(ws *Workspace) string {
	return "Run a Python 3.14 script inside a fresh, resource-limited Linux sandbox " +
		"container and return its stdout and stderr. Files under /work persist between " +
		"calls in this conversation for the duration of the job: a file written in one " +
		"call is still there in the next, so build on earlier work instead of redoing it. " +
		"Every call still starts a brand-new container, so running processes do NOT " +
		"persist — restart a server or re-run any setup you need. The " +
		"sandbox has outbound network access (urllib works), but is otherwise locked " +
		"down: no root, read-only root filesystem, 1 CPU, 256 MB RAM and a " +
		limitText(ws.Timeout) + " limit per call — nothing can be installed at run time (no " +
		"pip, apt-get or apk). Write files under the working directory /work: regular " +
		"files created or modified by this call are delivered to the user as " +
		"downloadable attachments (up to 20 files, 10 MB each; the image ships " +
		"reportlab, openpyxl, python-docx, Pillow and matplotlib). The CLI toolbelt is " +
		"available too: git, curl, wget, openssl, ripgrep (rg), jq, tree, file, less, " +
		"bc, zip/unzip, xz/bzip2/zstd, pdftotext (poppler-utils), sqlite3, ImageMagick " +
		"(magick/convert) and make. " + quotaNote(ws) + "Pass the script in `code`; it " +
		"is piped to `python -` on stdin."
}

// quotaNote tells the model about the disk cap only when one is enforced.
func quotaNote(ws *Workspace) string {
	if ws.QuotaBytes <= 0 {
		return ""
	}
	return "The workspace has a total disk quota; delete files you no longer need. "
}

// limitText renders the per-call wall-clock limit the tool actually has.
func limitText(d time.Duration) string {
	switch {
	case d <= 0:
		return "30-second"
	case d%time.Minute == 0:
		return fmt.Sprintf("%d-minute", d/time.Minute)
	default:
		return d.String()
	}
}

// runSandboxTool decodes the shared `code` argument, runs it, and renders the
// result. A non-nil ws binds the call to a persistent agent workspace. Produced
// files are reported through collect (when non-nil). A timeout is reported as a
// normal result carrying any partial output plus a note, so the model can see
// what happened; other failures propagate.
func runSandboxTool(ctx context.Context, r SandboxRunner, language string, args json.RawMessage, collect ArtifactCollector, ws *Workspace) (string, error) {
	if r == nil {
		return "", errors.New("code sandbox is not available")
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Code) == "" {
		return "", errors.New("code is required")
	}
	spec := sandbox.Spec{Language: language, Code: in.Code}
	if ws != nil {
		spec.Workspace = ws.Path
		spec.QuotaDir = ws.QuotaDir
		spec.QuotaBytes = ws.QuotaBytes
		spec.Timeout = ws.Timeout
	}
	res, err := r.Run(ctx, spec)
	if collect != nil {
		for _, a := range res.Artifacts {
			collect(a)
		}
	}
	out := formatSandboxResult(res)
	if errors.Is(err, sandbox.ErrTimeout) {
		return out + "\n\n[execution timed out — the sandbox was terminated and removed]", nil
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

// formatSandboxResult renders a sandbox run as a compact block for the model.
func formatSandboxResult(res sandbox.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit code: %d", res.ExitCode)
	if s := strings.TrimRight(res.Stdout, "\n"); s != "" {
		b.WriteString("\n--- stdout ---\n")
		b.WriteString(s)
	}
	if s := strings.TrimRight(res.Stderr, "\n"); s != "" {
		b.WriteString("\n--- stderr ---\n")
		b.WriteString(s)
	}
	if len(res.Artifacts) > 0 {
		b.WriteString("\n--- artifacts ---\n")
		for _, a := range res.Artifacts {
			fmt.Fprintf(&b, "%s (%s, %d bytes)\n", a.Name, a.MIME, len(a.Data))
		}
	}
	return b.String()
}
