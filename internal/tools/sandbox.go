package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// RunBashTool runs a Bash script in the sandbox (Phase 2.7).
type RunBashTool struct {
	r       SandboxRunner
	collect ArtifactCollector
}

// NewRunBashTool returns a run_bash tool backed by r.
func NewRunBashTool(r SandboxRunner) *RunBashTool { return &RunBashTool{r: r} }

// NewRunBashToolWithCollect returns a run_bash tool that reports produced files
// through collect (5.8). A nil collect behaves like NewRunBashTool.
func NewRunBashToolWithCollect(r SandboxRunner, collect ArtifactCollector) *RunBashTool {
	return &RunBashTool{r: r, collect: collect}
}

func (t *RunBashTool) Name() string { return "run_bash" }

func (t *RunBashTool) Description() string {
	return "Run a Bash script (bash 5.3) inside a fresh, ephemeral, resource-limited " +
		"Linux sandbox container and return its stdout and stderr. The filesystem does " +
		"NOT persist: every call starts a brand-new container, so files written in one " +
		"call are gone by the next. The sandbox has outbound network access (curl/wget " +
		"and apt-get work), but is otherwise locked down: no root, read-only root " +
		"filesystem, 1 CPU, 256 MB RAM and a 30-second limit. Write files under the " +
		"working directory /work: regular files left there when the script finishes are " +
		"delivered to the user as downloadable attachments (up to 20 files, 10 MB each). " +
		"Pass the script in `code`; it is piped to `bash -s` on stdin. Prefer " +
		"non-interactive, self-contained scripts that install any packages they need " +
		"within the same call."
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
	return runSandboxTool(ctx, t.r, sandbox.LanguageBash, args, t.collect)
}

// RunPythonTool runs a Python script in the sandbox (Phase 2.6).
type RunPythonTool struct {
	r       SandboxRunner
	collect ArtifactCollector
}

// NewRunPythonTool returns a run_python tool backed by r.
func NewRunPythonTool(r SandboxRunner) *RunPythonTool { return &RunPythonTool{r: r} }

// NewRunPythonToolWithCollect returns a run_python tool that reports produced
// files through collect (5.8). A nil collect behaves like NewRunPythonTool.
func NewRunPythonToolWithCollect(r SandboxRunner, collect ArtifactCollector) *RunPythonTool {
	return &RunPythonTool{r: r, collect: collect}
}

func (t *RunPythonTool) Name() string { return "run_python" }

func (t *RunPythonTool) Description() string {
	return "Run a Python 3.14 script inside a fresh, ephemeral, resource-limited Linux " +
		"sandbox container and return its stdout and stderr. The filesystem does NOT " +
		"persist: every call starts a brand-new container, so files written in one call " +
		"are gone by the next. The sandbox has outbound network access (urllib/requests " +
		"and `pip install` work), but is otherwise locked down: no root, read-only root " +
		"filesystem, 1 CPU, 256 MB RAM and a 30-second limit. Write files under the " +
		"working directory /work: regular files left there when the script finishes are " +
		"delivered to the user as downloadable attachments (up to 20 files, 10 MB each; " +
		"the image ships reportlab, openpyxl, python-docx, Pillow and matplotlib). Pass " +
		"the script in `code`; it is piped to `python -` on stdin. Install any " +
		"third-party packages you need inside the same script (e.g. " +
		"subprocess.run(['pip','install',...])), since nothing persists between calls."
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
	return runSandboxTool(ctx, t.r, sandbox.LanguagePython, args, t.collect)
}

// runSandboxTool decodes the shared `code` argument, runs it, and renders the
// result. Produced files are reported through collect (when non-nil). A timeout
// is reported as a normal result carrying any partial output plus a note, so the
// model can see what happened; other failures propagate.
func runSandboxTool(ctx context.Context, r SandboxRunner, language string, args json.RawMessage, collect ArtifactCollector) (string, error) {
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
	res, err := r.Run(ctx, sandbox.Spec{Language: language, Code: in.Code})
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
