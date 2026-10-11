package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"myogchat/internal/sandbox"
)

// fakeRunner records the last Spec and returns a canned Result.
type fakeRunner struct {
	spec  sandbox.Spec
	calls int
	res   sandbox.Result
	err   error
}

func (f *fakeRunner) Run(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	f.calls++
	f.spec = spec
	return f.res, f.err
}

func TestRunBashToolMetadata(t *testing.T) {
	tool := NewRunBashTool(&fakeRunner{})
	if tool.Name() != "run_bash" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "does NOT persist") {
		t.Error("description must warn that files do not persist")
	}
	if !strings.Contains(tool.Description(), "network") {
		t.Error("description must mention network access")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters not valid JSON: %v", err)
	}
}

func TestRunPythonToolMetadata(t *testing.T) {
	tool := NewRunPythonTool(&fakeRunner{})
	if tool.Name() != "run_python" {
		t.Errorf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "Python 3.14") {
		t.Error("description must mention Python 3.14")
	}
}

func TestRunBashToolRun(t *testing.T) {
	f := &fakeRunner{res: sandbox.Result{ExitCode: 0, Stdout: "hello\n"}}
	tool := NewRunBashTool(f)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"code":"echo hello"}`))
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if f.spec.Language != sandbox.LanguageBash || f.spec.Code != "echo hello" {
		t.Fatalf("spec = %+v", f.spec)
	}
	if !strings.Contains(out, "exit code: 0") || !strings.Contains(out, "hello") {
		t.Errorf("output = %q", out)
	}
}

func TestRunPythonToolRun(t *testing.T) {
	f := &fakeRunner{res: sandbox.Result{ExitCode: 0, Stdout: "42\n"}}
	tool := NewRunPythonTool(f)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"code":"print(42)"}`)); err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if f.spec.Language != sandbox.LanguagePython {
		t.Fatalf("language = %q, want python", f.spec.Language)
	}
}

func TestSandboxToolOutputShape(t *testing.T) {
	f := &fakeRunner{res: sandbox.Result{ExitCode: 1, Stdout: "out", Stderr: "boom"}}
	tool := NewRunBashTool(f)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"code":"false"}`))
	if err != nil {
		t.Fatalf("non-zero exit must not be a Go error: %v", err)
	}
	if !strings.Contains(out, "exit code: 1") ||
		!strings.Contains(out, "--- stdout ---") ||
		!strings.Contains(out, "--- stderr ---") {
		t.Errorf("output = %q", out)
	}
}

func TestSandboxToolEmptyCode(t *testing.T) {
	tool := NewRunBashTool(&fakeRunner{})
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"code":"  "}`)); err == nil {
		t.Error("empty code should error")
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("missing code should error")
	}
}

func TestSandboxToolInvalidArgs(t *testing.T) {
	tool := NewRunBashTool(&fakeRunner{})
	if _, err := tool.Run(context.Background(), json.RawMessage(`{bad`)); err == nil {
		t.Error("invalid args should error")
	}
}

func TestSandboxToolTimeout(t *testing.T) {
	f := &fakeRunner{res: sandbox.Result{TimedOut: true, Stdout: "partial"}, err: sandbox.ErrTimeout}
	tool := NewRunPythonTool(f)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"code":"import time; time.sleep(999)"}`))
	if err != nil {
		t.Fatalf("timeout must be reported as a result, not an error: %v", err)
	}
	if !strings.Contains(out, "partial") || !strings.Contains(out, "timed out") {
		t.Errorf("timeout output = %q", out)
	}
}

func TestSandboxToolRunnerError(t *testing.T) {
	f := &fakeRunner{err: sandbox.ErrImageMissing}
	tool := NewRunBashTool(f)
	_, err := tool.Run(context.Background(), json.RawMessage(`{"code":"echo x"}`))
	if !errors.Is(err, sandbox.ErrImageMissing) {
		t.Fatalf("err = %v, want ErrImageMissing", err)
	}
}

func TestSandboxToolNilRunner(t *testing.T) {
	tool := NewRunBashTool(nil)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"code":"echo x"}`)); err == nil {
		t.Error("nil runner should error")
	}
}

func TestSandboxToolArtifactListAndCollector(t *testing.T) {
	f := &fakeRunner{res: sandbox.Result{
		ExitCode:  0,
		Stdout:    "done\n",
		Artifacts: []sandbox.Artifact{{Name: "report.csv", MIME: "text/csv", Data: []byte("a,b\n")}},
	}}
	var collected []sandbox.Artifact
	tool := NewRunPythonToolWithCollect(f, func(a sandbox.Artifact) { collected = append(collected, a) })
	out, err := tool.Run(context.Background(), json.RawMessage(`{"code":"print(1)"}`))
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if !strings.Contains(out, "--- artifacts ---") || !strings.Contains(out, "report.csv") {
		t.Errorf("output missing artifact list: %q", out)
	}
	if len(collected) != 1 || collected[0].Name != "report.csv" {
		t.Fatalf("collector got %+v", collected)
	}
}

func TestSandboxToolNilCollectorIsSafe(t *testing.T) {
	f := &fakeRunner{res: sandbox.Result{Artifacts: []sandbox.Artifact{{Name: "x.txt", Data: []byte("x")}}}}
	tool := NewRunBashTool(f) // no collector
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"code":"true"}`)); err != nil {
		t.Fatalf("Run with no collector: %v", err)
	}
}
