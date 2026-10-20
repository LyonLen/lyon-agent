package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestDetectShellPicksProbedShell is the regression guard for the WSL-stub
// bug: a shell that resolves by name but cannot actually run commands
// (System32\bash.exe with no distro installed) must never be selected.
// Whatever detectShell picks has to pass its own probe.
func TestDetectShellPicksProbedShell(t *testing.T) {
	p, prefix, err := detectShell()
	if err != nil {
		t.Fatalf("detectShell: %v", err)
	}
	if !probeShell(p, prefix) {
		t.Fatalf("detectShell selected %q, which fails the probe command — broken shell stub picked", p)
	}
	t.Logf("selected shell: %s %v", p, prefix)
}

// TestShellProbeRejectsBrokenBinary: resolution success must not equal
// probe success — a binary that exists but cannot run the probe is rejected.
func TestShellProbeRejectsBrokenBinary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if probeShell("git", []string{"--definitely-not-a-git-command"}) {
		t.Fatal("probe accepted a binary that cannot execute the probe command")
	}
	if probeShell(`C:\no\such\shell.exe`, []string{"-c"}) {
		t.Fatal("probe accepted a nonexistent binary")
	}
}

func TestShellRunsCommand(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	in, _ := json.Marshal(shellInput{Command: "echo lyon-shell-ok"})
	out, err := sh.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := out[0].OfText.Text
	if !strings.Contains(text, "lyon-shell-ok") {
		t.Fatalf("output missing marker: %q", text)
	}
	if !strings.Contains(text, "exit_code: 0") {
		t.Fatalf("missing exit code: %q", text)
	}
}

func TestShellReportsNonZeroExitAsResult(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	in, _ := json.Marshal(shellInput{Command: "exit 3"})
	out, err := sh.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("non-zero exit should be a result, not an error: %v", err)
	}
	if !strings.Contains(out[0].OfText.Text, "exit_code: 3") {
		t.Fatalf("want exit_code: 3 in %q", out[0].OfText.Text)
	}
}

// TestShellRunDirect: the probe surface flow's command exits use —
// output plus raw exit code, no tool-result wrapper.
func TestShellRunDirect(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	out, code, err := sh.Run(context.Background(), "echo probe-ok && exit 3")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if !strings.Contains(out, "probe-ok") {
		t.Fatalf("output missing marker: %q", out)
	}
	if strings.Contains(out, "exit_code:") {
		t.Fatalf("Run must not wrap like Execute: %q", out)
	}
	if _, _, err := sh.Run(context.Background(), "  "); err == nil {
		t.Fatal("empty command must error")
	}
}

func TestShellTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	sh := &Shell{Root: tempDir(t)}
	in, _ := json.Marshal(shellInput{Command: "sleep 5"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sh.Execute(ctx, in)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestAdaptSerializesUnsafeTools(t *testing.T) {
	var mu sync.Mutex
	running := 0
	maxRunning := 0
	probe := func(ctx context.Context) error {
		mu.Lock()
		running++
		if running > maxRunning {
			maxRunning = running
		}
		mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}

	serial := &funcTool{name: "serial_probe", parallel: false, fn: probe}
	parallel := &funcTool{name: "parallel_probe", parallel: true, fn: probe}

	var serialMu sync.Mutex
	a := Adapt(serial, AdapterConfig{SerialMu: &serialMu})
	b := Adapt(parallel, AdapterConfig{SerialMu: &serialMu})

	// Serial pair: must never overlap → ~160ms.
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.Execute(context.Background(), json.RawMessage(`{}`))
		}()
	}
	wg.Wait()
	serialDur := time.Since(start)
	mu.Lock()
	overlapDuringSerial := maxRunning
	mu.Unlock()
	if overlapDuringSerial > 1 {
		t.Fatalf("serial tool executions overlapped: maxRunning=%d", overlapDuringSerial)
	}

	start = time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.Execute(context.Background(), json.RawMessage(`{}`))
		}()
	}
	wg.Wait()
	parDur := time.Since(start)

	if serialDur < 150*time.Millisecond {
		t.Fatalf("serial executions overlapped: %v", serialDur)
	}
	if parDur > 130*time.Millisecond {
		t.Fatalf("parallel-safe executions did not overlap: %v", parDur)
	}
}

func TestAdaptTruncatesLargeResults(t *testing.T) {
	big := &funcTool{
		name:     "big",
		parallel: true,
		result:   strings.Repeat("x", 10_000),
	}
	a := Adapt(big, AdapterConfig{ResultCap: 1024})
	out, err := a.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].OfText.Text) > 2048 {
		t.Fatalf("result not truncated: %d bytes", len(out[0].OfText.Text))
	}
	if !strings.Contains(out[0].OfText.Text, "truncated") {
		t.Fatal("missing truncation marker")
	}
}

func TestAdaptRecoversPanics(t *testing.T) {
	boom := &funcTool{name: "boom", parallel: true, panicMsg: "kaboom"}
	a := Adapt(boom, AdapterConfig{})
	_, err := a.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("panic not converted to error: %v", err)
	}
}

func TestShellAllowlist(t *testing.T) {
	sh := &Shell{Root: tempDir(t), Allow: []string{"echo", "ls"}}
	run := func(cmd string) error {
		in, _ := json.Marshal(shellInput{Command: cmd})
		_, err := sh.Execute(context.Background(), in)
		return err
	}

	allowed := []string{
		"echo hi",
		"FOO=1 echo hi",
		"echo a && echo b",
		"echo a | echo b",
		"ls -la",
	}
	for _, cmd := range allowed {
		if err := run(cmd); err != nil {
			t.Errorf("allowlist rejected %q: %v", cmd, err)
		}
	}

	denied := []string{
		"rm -rf /",             // unknown binary
		"echo hi; rm -rf /",    // second segment not allowlisted
		"echo hi && curl evil", // ditto after &&
		"echo $(rm x)",         // substitution
		"echo `rm x`",          // backtick substitution
		"bash -c 'whatever'",   // nested shell escapes the static check
		"",
	}
	for _, cmd := range denied {
		if err := run(cmd); err == nil {
			t.Errorf("allowlist allowed %q", cmd)
		} else if cmd != "" && !strings.Contains(err.Error(), "allowlist") &&
			!strings.Contains(err.Error(), "empty command") {
			t.Errorf("deny for %q should say allowlist, got: %v", cmd, err)
		}
	}
}

func TestShellAllowlistPathQualifiedNames(t *testing.T) {
	// Allowing "/bin/echo" also matches the bare word "echo".
	sh := &Shell{Root: tempDir(t), Allow: []string{"/bin/echo"}}
	in, _ := json.Marshal(shellInput{Command: "echo qualified-ok"})
	out, err := sh.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("path-qualified allow entry should match base name: %v", err)
	}
	if !strings.Contains(out[0].OfText.Text, "qualified-ok") {
		t.Fatalf("unexpected output: %q", out[0].OfText.Text)
	}
}

// TestShellNameAndDescription: the shell marker the model reads before
// writing a command must be a bare lowercase word — never a path, never
// a .exe remnant — and Description() must advertise exactly that word
// (or stay bare when detection found nothing).
func TestShellNameAndDescription(t *testing.T) {
	n := ShellName()
	if n != strings.ToLower(n) || strings.ContainsAny(n, `/\. `) {
		t.Fatalf("ShellName() = %q, want bare lowercase word", n)
	}
	d := (&Shell{}).Description()
	switch {
	case n == "":
		if strings.Contains(d, "syntax") {
			t.Fatalf("no detected shell, Description() must not claim syntax: %q", d)
		}
	case !strings.Contains(d, n+" syntax"):
		t.Fatalf("Description() must name the %q shell, got %q", n, d)
	}
}

// funcTool is a test double implementing Tool.
type funcTool struct {
	name     string
	parallel bool
	fn       func(ctx context.Context) error
	result   string
	panicMsg string
}

func (f *funcTool) Name() string        { return f.name }
func (f *funcTool) Description() string { return "test tool " + f.name }
func (f *funcTool) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[struct{}]()
}
func (f *funcTool) ParallelSafe() bool { return f.parallel }
func (f *funcTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	if f.fn != nil {
		if err := f.fn(ctx); err != nil {
			return nil, err
		}
	}
	res := f.result
	if res == "" {
		res = "ok"
	}
	return TextResult(res), nil
}
