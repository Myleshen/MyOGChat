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

// RunBashTool runs a Bash script in the sandbox (Phase 2.7).
type RunBashTool struct{ r SandboxRunner }

// NewRunBashTool returns a run_bash tool backed by r.
func NewRunBashTool(r SandboxRunner) *RunBashTool { return &RunBashTool{r: r} }

func (t *RunBashTool) Name() string { return "run_bash" }

func (t *RunBashTool) Description() string {
	return "Run a Bash script (bash 5.3) inside a fresh, ephemeral, resource-limited " +
		"Linux sandbox container and return its stdout and stderr. The filesystem does " +
		"NOT persist: every call starts a brand-new container, so files written in one " +
		"call are gone by the next. The sandbox has outbound network access (curl/wget " +
		"and apt-get work), but is otherwise locked down: no root, read-only root " +
		"filesystem, 1 CPU, 256 MB RAM and a 30-second limit. Write files under the " +
		"working directory /work (a writable tmpfs). Pass the script in `code`; it is " +
		"piped to `bash -s` on stdin. Prefer non-interactive, self-contained scripts " +
		"that install any packages they need within the same call."
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
	return runSandboxTool(ctx, t.r, sandbox.LanguageBash, args)
}

// RunPythonTool runs a Python script in the sandbox (Phase 2.6).
type RunPythonTool struct{ r SandboxRunner }

// NewRunPythonTool returns a run_python tool backed by r.
func NewRunPythonTool(r SandboxRunner) *RunPythonTool { return &RunPythonTool{r: r} }

func (t *RunPythonTool) Name() string { return "run_python" }

func (t *RunPythonTool) Description() string {
	return "Run a Python 3.14 script inside a fresh, ephemeral, resource-limited Linux " +
		"sandbox container and return its stdout and stderr. The filesystem does NOT " +
		"persist: every call starts a brand-new container, so files written in one call " +
		"are gone by the next. The sandbox has outbound network access (urllib/requests " +
		"and `pip install` work), but is otherwise locked down: no root, read-only root " +
		"filesystem, 1 CPU, 256 MB RAM and a 30-second limit. Write files under the " +
		"working directory /work (a writable tmpfs). Pass the script in `code`; it is " +
		"piped to `python -` on stdin. Install any third-party packages you need inside " +
		"the same script (e.g. subprocess.run(['pip','install',...])), since nothing " +
		"persists between calls."
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
	return runSandboxTool(ctx, t.r, sandbox.LanguagePython, args)
}

// runSandboxTool decodes the shared `code` argument, runs it, and renders the
// result. A timeout is reported as a normal result carrying any partial output
// plus a note, so the model can see what happened; other failures propagate.
func runSandboxTool(ctx context.Context, r SandboxRunner, language string, args json.RawMessage) (string, error) {
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
	return b.String()
}
