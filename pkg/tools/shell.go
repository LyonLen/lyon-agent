package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// shellOutputLimit is the maximum output returned to the model from one
// shell invocation, in bytes. The adapter's ResultCap is a second line of
// defense; this keeps the message itself small.
const shellOutputLimit = 32 * 1024

// Shell runs a command inside the workspace. It is a serial (mutating) tool:
// commands share the workspace filesystem, so the adapter never overlaps them.
type Shell struct {
	// Root is the working directory and confinement boundary for commands.
	Root string
	// Allow, when non-empty, is a best-effort command allowlist: every
	// command segment (split on ; | & and newlines) must begin with one of
	// these names, and shell substitution ($(), backticks, process
	// substitution) is rejected outright. It fails closed — quoting edge
	// cases deny rather than allow. Not a security sandbox; it raises the
	// cost of accidental dangerous commands.
	Allow []string
}

func (s *Shell) Name() string { return "run_shell" }

func (s *Shell) Description() string {
	return "Run a shell command in the workspace directory. Returns the exit code and combined stdout+stderr (truncated to 32KB). Use for file operations, builds, and anything better expressed as a command."
}

func (s *Shell) ParallelSafe() bool { return false }

// NeedsApproval implements Approvable: shell commands can do anything the
// user can, so they ask first when an approval hook is configured.
func (s *Shell) NeedsApproval() bool { return true }

type shellInput struct {
	Command string `json:"command" jsonschema:"required,description=The shell command to execute"`
}

func (s *Shell) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[shellInput]()
}

var (
	shellOnce   sync.Once
	shellPath   string
	shellPrefix []string
	shellErr    error
)

// Detection runs probeCommand in each candidate shell and requires the
// marker back: LookPath alone is not trustworthy on Windows, where the WSL
// stub (System32\bash.exe) resolves fine but fails at runtime with
// "Windows Subsystem for Linux is not installed" whenever no distro exists.
const (
	probeCommand = "echo lyon-shell-probe-ok"
	probeOutput  = "lyon-shell-probe-ok"
)

// probeShell reports whether path can run prefix + probeCommand and echo
// the marker back with exit status 0.
func probeShell(path string, prefix []string) bool {
	args := append(append([]string{}, prefix...), probeCommand)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	return err == nil && strings.Contains(string(out), probeOutput)
}

// resolveShellPath resolves a bare command name via PATH, or validates that
// an absolute path exists (LookPath's absolute-path branch is picky on
// Windows about relative segments and separators).
func resolveShellPath(p string) (string, error) {
	if filepath.IsAbs(p) {
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
	return exec.LookPath(p)
}

// detectShell picks a working shell once per process. Every candidate is
// probed for actual execution, not just name resolution. On Windows the
// order is: bash on PATH (a working WSL/Git bash keeps priority), the usual
// Git for Windows install paths (Git's bin dir is typically NOT on PATH),
// then pwsh, powershell, cmd.
func detectShell() (string, []string, error) {
	shellOnce.Do(func() {
		type candidate struct {
			path string
			args []string
		}
		var order []candidate
		if runtime.GOOS == "windows" {
			order = []candidate{
				{"bash", []string{"-c"}},
			}
			for _, root := range []string{
				os.Getenv("ProgramFiles"),
				os.Getenv("ProgramFiles(x86)"),
				os.Getenv("LOCALAPPDATA"),
			} {
				if root == "" {
					continue
				}
				order = append(order, candidate{
					path: filepath.Join(root, "Git", "bin", "bash.exe"),
					args: []string{"-c"},
				})
			}
			order = append(order,
				candidate{"pwsh", []string{"-NoProfile", "-NonInteractive", "-Command"}},
				candidate{"powershell", []string{"-NoProfile", "-NonInteractive", "-Command"}},
				candidate{"cmd", []string{"/c"}},
			)
		} else {
			order = []candidate{
				{"bash", []string{"-c"}},
				{"sh", []string{"-c"}},
			}
		}
		var tried []string
		for _, c := range order {
			p, err := resolveShellPath(c.path)
			if err == nil && probeShell(p, c.args) {
				shellPath, shellPrefix, shellErr = p, c.args, nil
				return
			}
			tried = append(tried, c.path)
		}
		shellErr = fmt.Errorf("no working shell found (tried: %s)", strings.Join(tried, ", "))
	})
	return shellPath, shellPrefix, shellErr
}

func (s *Shell) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in shellInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return nil, fmt.Errorf("empty command")
	}
	out, code, truncated, err := s.run(ctx, in.Command)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", code)
	if truncated {
		fmt.Fprintf(&b, "output (truncated at %d bytes):\n", shellOutputLimit)
	}
	b.Write(out)
	return TextResult(b.String()), nil
}

// Run executes one command and reports output plus exit code directly —
// the probe surface flow's command exits hang off (they want a verdict,
// not a tool-result wrapper). Truncation is applied silently here: probes
// read exit codes and short verdicts, not the full stream.
func (s *Shell) Run(ctx context.Context, command string) (string, int, error) {
	if strings.TrimSpace(command) == "" {
		return "", 0, fmt.Errorf("empty command")
	}
	out, code, _, err := s.run(ctx, command)
	if err != nil {
		return "", 0, err
	}
	return string(out), code, nil
}

// streamCoalesce bounds the live-output event rate (#11): pending bytes
// ship at most this often while a command streams, so a 10k-line/s
// build becomes ≤25 events/s instead of pushing the event channel into
// backpressure against the process's own output pipe.
const streamCoalesce = 40 * time.Millisecond

// streamPendCap bounds a newline-free stream (progress bars): past it,
// pending bytes flush as-is rather than buffering forever.
const streamPendCap = 16 << 10

// streamBuf captures command output for the tool result while tee-ing
// the same bytes to the live output sink (tools.WithOutputSink, #11).
// Writes come from exec's copier goroutine — one at a time for a shared
// Stdout/Stderr writer — plus at most one coalescing timer goroutine;
// the mutex covers both.
type streamBuf struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	pend   []byte
	sink   func(string)
	last   time.Time
	timer  *time.Timer
	closed bool
}

func (w *streamBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.closed {
		return len(p), nil // late copier tail: result bytes only
	}
	w.pend = append(w.pend, p...)
	if len(w.pend) >= streamPendCap || time.Since(w.last) >= streamCoalesce {
		w.flushLocked()
	} else if w.timer == nil {
		// Within the window: hold, and wake once to ship the tail even
		// if the command falls silent (print, then think for seconds).
		w.timer = time.AfterFunc(streamCoalesce, w.lateFlush)
	}
	return len(p), nil
}

// lateFlush runs on the coalescing timer goroutine.
func (w *streamBuf) lateFlush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer = nil
	if !w.closed {
		w.flushLocked()
	}
}

// flushLocked ships the pending bytes to the sink (callers hold w.mu).
// string(w.pend) copies, so recycling the buffer is safe.
func (w *streamBuf) flushLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if len(w.pend) == 0 {
		return
	}
	chunk := string(w.pend)
	w.pend = w.pend[:0]
	w.last = time.Now()
	if w.sink != nil {
		w.sink(chunk) // must not block long: it runs under w.mu
	}
}

// Close ships the final tail BEFORE OnToolEnd can fire downstream, and
// silences any armed timer — no output event may trail the end event.
func (w *streamBuf) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.flushLocked()
}

// run is the shared execution body: allowlisted spawn, bounded output.
func (s *Shell) run(ctx context.Context, command string) ([]byte, int, bool, error) {
	sh, prefix, err := detectShell()
	if err != nil {
		return nil, 0, false, err
	}
	if len(s.Allow) > 0 {
		if err := checkAllowlist(command, s.Allow); err != nil {
			return nil, 0, false, err
		}
	}

	args := append(append([]string{}, prefix...), command)
	cmd := exec.CommandContext(ctx, sh, args...)
	// Without WaitDelay, killing the shell on timeout leaves grandchildren
	// (e.g. `sleep`) holding the output pipes and CombinedOutput blocks
	// until they exit. WaitDelay bounds that wait after cancellation.
	cmd.WaitDelay = time.Second
	if s.Root != "" {
		cmd.Dir = s.Root
	}

	// One writer for both streams: capture for the result, tee live
	// chunks to the sink when a session is listening (#11). Nil sink
	// degrades to plain capture — probes and metrics pay nothing.
	out := &streamBuf{sink: OutputSink(ctx)}
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()
	out.Close() // tail flush lands before any OnToolEnd downstream

	code := 0
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, 0, false, fmt.Errorf("command timed out or was cancelled: %w", ctx.Err())
		}
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return nil, 0, false, fmt.Errorf("failed to run %s: %w", sh, runErr)
		}
	}

	bytesOut := out.buf.Bytes()
	truncated := false
	if len(bytesOut) > shellOutputLimit {
		bytesOut = bytesOut[:shellOutputLimit]
		truncated = true
	}
	return bytesOut, code, truncated, nil
}

// checkAllowlist statically screens a command against the allowlist. It
// fails closed: substitution constructs are rejected outright, segments are
// split on shell control operators, and a segment whose first word is not
// allowlisted denies the whole command. Leading VAR=value assignments are
// skipped when finding the first word.
func checkAllowlist(cmd string, allow []string) error {
	if strings.Contains(cmd, "$(") || strings.Contains(strings.TrimSpace(cmd), "`") ||
		strings.Contains(cmd, "<(") {
		return fmt.Errorf("command blocked by allowlist: shell substitution is not allowed")
	}
	set := make(map[string]struct{}, len(allow)*2)
	for _, a := range allow {
		set[a] = struct{}{}
		set[filepath.Base(a)] = struct{}{} // "/bin/ls" allowlists "ls" too
	}

	// FieldsFunc splits on every control operator; empty fields (e.g. from
	// "||") are dropped, so each field is one command segment.
	segments := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '|' || r == '&' || r == '\n'
	})
	for _, seg := range segments {
		fields := strings.Fields(seg)
		// Skip leading environment assignments: FOO=bar cmd ...
		for len(fields) > 0 && isEnvAssign(fields[0]) {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue // segment was only assignments — nothing to execute
		}
		first := fields[0]
		if _, ok := set[first]; ok {
			continue
		}
		if _, ok := set[filepath.Base(first)]; ok {
			continue
		}
		return fmt.Errorf("command blocked by allowlist: %q is not in the allowlist", first)
	}
	return nil
}

// isEnvAssign reports whether a shell word looks like NAME=value (with NAME
// a plausible identifier), used to skip assignments before the command word.
func isEnvAssign(field string) bool {
	eq := strings.IndexByte(field, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := field[i]
		if !(c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && i > 0 && c <= '9')) {
			return false
		}
	}
	return true
}
