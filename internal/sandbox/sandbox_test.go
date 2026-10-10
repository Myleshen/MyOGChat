package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// record is one captured executor invocation.
type record struct {
	name  string
	args  []string
	stdin []byte
}

// fakeExec is an injectable ExecFunc that records calls and delegates to fn.
type fakeExec struct {
	mu    sync.Mutex
	calls []record
	fn    func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error)
}

func (f *fakeExec) run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, record{name: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, name, args, stdin)
	}
	return nil, nil, nil
}

func (f *fakeExec) find(sub string) []record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []record
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == sub {
			out = append(out, c)
		}
	}
	return out
}

// newTestRunner returns a runner whose network inspect succeeds, so only the
// run/cleanup calls matter to most tests.
func newTestRunner(cfg Config, f *fakeExec) *Runner {
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "network" && len(args) > 1 && args[1] == "inspect" {
			return []byte("ok"), nil, nil
		}
		return nil, nil, nil
	}
	return NewRunnerWithExec(cfg, f.run)
}

func testConfig() Config {
	c := DefaultConfig()
	c.Docker = "/usr/bin/docker"
	c.Network = "myogchat-sbx"
	c.BashImage = "bash:5.3"
	c.PythonImage = "python:3.14-slim"
	c.Timeout = 5 * time.Second
	c.MaxOutput = DefaultMaxOutput
	return c
}

func TestRunArgsBash(t *testing.T) {
	r := NewRunner(testConfig())
	got := r.runArgs("bash:5.3", []string{"bash", "-s"}, "myogchat-sbx-abc")
	want := []string{
		"run",
		"--rm",
		"--name", "myogchat-sbx-abc",
		"--label", "myogchat-sandbox=1",
		"--network", "myogchat-sbx",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"--read-only",
		"--tmpfs", "/work:rw,exec,size=64m",
		"--tmpfs", "/tmp:rw,size=16m",
		"--workdir", "/work",
		"--memory", "256m",
		"--memory-swap", "256m",
		"--cpus", "1",
		"--pids-limit", "128",
		"--ulimit", "nofile=256:256",
		"--stop-timeout", "2",
		"--pull", "never",
		"-e", "HOME=/work",
		"-e", "TMPDIR=/tmp",
		"-i",
		"bash:5.3",
		"bash", "-s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runArgs mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunArgsPython(t *testing.T) {
	r := NewRunner(testConfig())
	got := r.runArgs("python:3.14-slim", []string{"python", "-"}, "myogchat-sbx-xyz")
	// The command must read from stdin, never carry the code itself.
	if got[len(got)-2] != "python" || got[len(got)-1] != "-" {
		t.Fatalf("python command = %q, want [python -]", got[len(got)-2:])
	}
	if got[len(got)-3] != "python:3.14-slim" {
		t.Fatalf("image = %q", got[len(got)-3])
	}
}

// TestRunFeedsCodeOnStdinAndMapsExitCode asserts the code goes over stdin (not
// argv) and a non-zero container exit becomes Result.ExitCode, not an error.
func TestRunFeedsCodeOnStdinAndMapsExitCode(t *testing.T) {
	f := &fakeExec{}
	r := newTestRunner(testConfig(), f)
	exitErr := exec.Command("sh", "-c", "exit 3").Run()
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		switch args[0] {
		case "network":
			return nil, nil, nil
		case "run":
			return []byte("out\n"), []byte("err\n"), exitErr
		}
		return nil, nil, nil
	}

	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "echo hi"})
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Errorf("stdout/stderr = %q/%q", res.Stdout, res.Stderr)
	}
	runs := f.find("run")
	if len(runs) != 1 {
		t.Fatalf("run calls = %d, want 1", len(runs))
	}
	if string(runs[0].stdin) != "echo hi" {
		t.Errorf("stdin = %q, want the code", runs[0].stdin)
	}
	// The code must never appear in argv.
	for _, a := range runs[0].args {
		if strings.Contains(a, "echo hi") {
			t.Errorf("code leaked into argv: %q", a)
		}
	}
}

func TestRunTimeoutCleansUp(t *testing.T) {
	f := &fakeExec{}
	cfg := testConfig()
	cfg.Timeout = 40 * time.Millisecond
	r := newTestRunner(cfg, f)
	f.fn = func(ctx context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		switch args[0] {
		case "network":
			return nil, nil, nil
		case "run":
			<-ctx.Done()
			return []byte("partial"), nil, ctx.Err()
		}
		return nil, nil, nil
	}

	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "sleep 100"})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !res.TimedOut {
		t.Error("Result.TimedOut = false, want true")
	}
	if res.Stdout != "partial" {
		t.Errorf("partial stdout lost: %q", res.Stdout)
	}

	runs := f.find("run")
	if len(runs) != 1 {
		t.Fatalf("run calls = %d, want 1", len(runs))
	}
	name := argValue(runs[0].args, "--name")
	if name == "" {
		t.Fatal("no --name in run args")
	}
	// Cleanup must force-remove the same container.
	rm := f.find("rm")
	if len(rm) != 1 || !reflect.DeepEqual(rm[0].args, []string{"rm", "-f", name}) {
		t.Fatalf("cleanup calls = %v, want [rm -f %s]", rm, name)
	}
}

func TestRunTruncation(t *testing.T) {
	f := &fakeExec{}
	cfg := testConfig()
	cfg.MaxOutput = 10
	r := newTestRunner(cfg, f)
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if args[0] == "run" {
			return []byte("0123456789ABCDEF"), []byte("stderr"), nil
		}
		return nil, nil, nil
	}
	res, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"})
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	if res.Stdout != "0123456789"+truncMarker {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "" {
		t.Errorf("stderr should be dropped when stdout fills the budget: %q", res.Stderr)
	}
}

func TestRunTruncationKeepsStderrInBudget(t *testing.T) {
	f := &fakeExec{}
	cfg := testConfig()
	cfg.MaxOutput = 10
	r := newTestRunner(cfg, f)
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if args[0] == "run" {
			return []byte("abc"), []byte("0123456789XYZ"), nil
		}
		return nil, nil, nil
	}
	res, _ := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"})
	if res.Stdout != "abc" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "0123456"+truncMarker {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		execEr error
		want   error
	}{
		{"image missing", "Unable to find image 'bash:5.3' locally\npull access denied", errors.New("exit status 125"), ErrImageMissing},
		{"network missing", "Error response from daemon: network myogchat-sbx not found", errors.New("exit status 125"), ErrNetworkMissing},
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.", errors.New("exit status 1"), ErrDockerUnavailable},
		{"permission", "permission denied while trying to connect to the docker API", errors.New("exit status 1"), ErrDockerUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{}
			r := newTestRunner(testConfig(), f)
			f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
				if args[0] == "run" {
					return nil, []byte(tc.stderr), tc.execEr
				}
				return nil, nil, nil
			}
			_, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunDockerNotFound(t *testing.T) {
	f := &fakeExec{}
	r := newTestRunner(testConfig(), f)
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if args[0] == "run" {
			return nil, nil, exec.ErrNotFound
		}
		return nil, nil, nil
	}
	_, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"})
	if !errors.Is(err, ErrDockerNotFound) {
		t.Fatalf("err = %v, want ErrDockerNotFound", err)
	}
}

func TestRunValidation(t *testing.T) {
	f := &fakeExec{}
	r := newTestRunner(testConfig(), f)
	if _, err := r.Run(context.Background(), Spec{Language: "ruby", Code: "puts 1"}); err == nil {
		t.Error("unknown language should error")
	}
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "   "}); err == nil {
		t.Error("empty code should error")
	}
}

func TestEnsureNetworkCreatesOnce(t *testing.T) {
	f := &fakeExec{}
	r := NewRunnerWithExec(testConfig(), f.run)
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		switch {
		case args[0] == "network" && args[1] == "inspect":
			return nil, []byte("no such network"), errors.New("exit status 1")
		case args[0] == "network" && args[1] == "create":
			return []byte("net"), nil, nil
		case args[0] == "run":
			return nil, nil, nil
		}
		return nil, nil, nil
	}

	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "echo 1"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "echo 2"}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := len(f.find("network")); got != 2 {
		t.Fatalf("network calls = %d, want 2 (one inspect + one create)", got)
	}
	if len(f.find("run")) != 2 {
		t.Fatalf("run calls = %d, want 2", len(f.find("run")))
	}
}

func TestEnsureNetworkFailure(t *testing.T) {
	f := &fakeExec{}
	r := NewRunnerWithExec(testConfig(), f.run)
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if args[0] == "network" {
			return nil, []byte("cannot create"), errors.New("exit status 1")
		}
		return nil, nil, nil
	}
	_, err := r.Run(context.Background(), Spec{Language: LanguageBash, Code: "x"})
	if !errors.Is(err, ErrNetworkMissing) {
		t.Fatalf("err = %v, want ErrNetworkMissing", err)
	}
}

func TestAvailableProbeCaches(t *testing.T) {
	f := &fakeExec{}
	r := NewRunnerWithExec(testConfig(), f.run)
	calls := 0
	f.fn = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if args[0] == "info" {
			calls++
			return []byte("29.0.0"), nil, nil
		}
		return nil, nil, nil
	}
	if !r.Available(context.Background()) {
		t.Fatal("Available = false, want true")
	}
	if !r.Available(context.Background()) {
		t.Fatal("second Available = false")
	}
	if calls != 1 {
		t.Errorf("info probe calls = %d, want 1 (cached)", calls)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("MYOGCHAT_DOCKER", "/opt/docker")
	t.Setenv("MYOGCHAT_SANDBOX_NETWORK", "custom-net")
	t.Setenv("MYOGCHAT_SANDBOX_IMAGE_BASH", "bash:5.2")
	t.Setenv("MYOGCHAT_SANDBOX_IMAGE_PYTHON", "python:3.12-slim")
	t.Setenv("MYOGCHAT_SANDBOX_MEMORY", "512m")
	t.Setenv("MYOGCHAT_SANDBOX_CPUS", "2")
	t.Setenv("MYOGCHAT_SANDBOX_TIMEOUT", "45")
	t.Setenv("MYOGCHAT_SANDBOX_MAX_OUTPUT", "1234")
	c := ConfigFromEnv()
	if c.Docker != "/opt/docker" || c.Network != "custom-net" || c.BashImage != "bash:5.2" ||
		c.PythonImage != "python:3.12-slim" || c.Memory != "512m" || c.CPUs != "2" ||
		c.Timeout != 45*time.Second || c.MaxOutput != 1234 {
		t.Fatalf("ConfigFromEnv = %+v", c)
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("MYOGCHAT_DOCKER", "")
	t.Setenv("MYOGCHAT_SANDBOX_TIMEOUT", "not-a-number")
	c := ConfigFromEnv()
	if c.Network != DefaultNetwork || c.BashImage != DefaultBashImage || c.PythonImage != DefaultPythonImage {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Timeout != DefaultTimeout {
		t.Errorf("invalid timeout should fall back to default, got %s", c.Timeout)
	}
}

func TestEnabledFromEnv(t *testing.T) {
	for _, v := range []string{"", "1", "true", "on", "yes", "banana"} {
		t.Setenv("MYOGCHAT_SANDBOX", v)
		if !EnabledFromEnv() {
			t.Errorf("EnabledFromEnv(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"0", "false", "off", "no", "OFF"} {
		t.Setenv("MYOGCHAT_SANDBOX", v)
		if EnabledFromEnv() {
			t.Errorf("EnabledFromEnv(%q) = true, want false", v)
		}
	}
}

// argValue returns the value following flag in args.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
