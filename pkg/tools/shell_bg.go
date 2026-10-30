// Background shell support: run_shell with background=true starts the
// command detached — no wall clock, output to a log file inside the
// workspace (read_file can page it), completion reported through the
// owner seam. The session binds itself via WithBackgroundOwner: on exit
// it posts a background_end event for the chat and hands the tail to the
// model (gap injection while a run is live, a system note when idle).
//
// Lifetime is a kill scope per task (Windows: a job object with
// KILL_ON_JOB_CLOSE; POSIX: its own process group — see the per-OS
// shell_bg_*.go). The handle lives in the registry entry, so even a
// hard-killed lyond (Windows taskkill /F) drops the handle and the job
// takes the whole tree down with it — orphan processes are structurally
// impossible while attach succeeded. Session close and prompt interrupt
// call KillBackgroundOwner; engine shutdown sweeps with KillAllBackground.
//
// Registry discipline: every handle operation (kill, release) runs under
// bgMu, and the waiter unregisters under the same lock before releasing,
// so a kill can never touch a handle another goroutine just closed.
package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// bgTailLimit bounds the output tail carried on the completion notice —
// enough to judge the outcome; the full stream stays in the log file.
const bgTailLimit = 2048

// BgInfo is one background task's completion report — the payload the
// owner's onEnd callback receives exactly once (after unregistering, so
// the registry is already clean when the notification lands).
type BgInfo struct {
	ID        int
	PID       int
	Log       string
	Command   string
	Code      int
	Err       string // Wait's error when it is not a plain exit
	ElapsedMS int64
	Tail      string
}

type bgOwnerKey struct{}

// bgOwner is a session's binding to its background tasks: an identity
// key for teardown and the completion callback.
type bgOwner struct {
	key   string
	onEnd func(BgInfo)
}

// WithBackgroundOwner binds the session's key and completion callback to
// the driving context so startBackground can attribute the task. Tools
// reached through other contexts (probes, tests) get no owner: the task
// still runs, but only KillAllBackground can reach it.
func WithBackgroundOwner(ctx context.Context, key string, onEnd func(BgInfo)) context.Context {
	return context.WithValue(ctx, bgOwnerKey{}, &bgOwner{key: key, onEnd: onEnd})
}

func bgOwnerOf(ctx context.Context) *bgOwner {
	o, _ := ctx.Value(bgOwnerKey{}).(*bgOwner)
	return o
}

type bgEntry struct {
	id      int
	key     string
	pid     int
	log     string
	command string
	started time.Time
	job     uintptr // Windows job handle (0 elsewhere, or before attach)
	proc    *exec.Cmd
}

var (
	bgMu   sync.Mutex
	bgSeq  int
	bgByID = map[int]*bgEntry{}
)

// bgLogDir is where a background command's output streams: inside the
// workspace when there is one (.lyon/ is gitignored and inside the
// read_file confinement, so the model can page it), else the home store.
func bgLogDir(root string) string {
	if root != "" {
		return filepath.Join(root, ".lyon", "shell-bg")
	}
	home := os.Getenv("LYON_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".lyon")
		} else {
			home = os.TempDir()
		}
	}
	return filepath.Join(home, "shell-bg")
}

// startBackground spawns the command detached and returns immediately —
// the tool result tells the model where the log is and how completion
// will arrive; there is no live output stream (OutputSink stays quiet).
func (s *Shell) startBackground(ctx context.Context, command string) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	sh, prefix, err := detectShell()
	if err != nil {
		return nil, err
	}
	if len(s.Allow) > 0 {
		if err := checkAllowlist(command, s.Allow); err != nil {
			return nil, err
		}
	}
	owner := bgOwnerOf(ctx)

	bgMu.Lock()
	bgSeq++
	id := bgSeq
	bgMu.Unlock()

	dir := bgLogDir(s.Root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("background log dir: %w", err)
	}
	logPath := filepath.Join(dir, fmt.Sprintf("bg-%d-%04d.log", os.Getpid(), id))
	f, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("background log: %w", err)
	}

	// Deliberately NOT CommandContext: the whole point is outliving the
	// tool call's ctx. Lifetime is owned by the kill scope below instead.
	args := append(append([]string{}, prefix...), command)
	cmd := exec.Command(sh, args...)
	if s.Root != "" {
		cmd.Dir = s.Root
	}
	cmd.Stdout = f
	cmd.Stderr = f
	setupBg(cmd)
	if err := cmd.Start(); err != nil {
		f.Close()
		os.Remove(logPath)
		return nil, fmt.Errorf("failed to start background %s: %w", sh, err)
	}
	f.Close() // the child owns its handle now

	e := &bgEntry{
		id: id, pid: cmd.Process.Pid, log: logPath, command: command,
		started: time.Now(), proc: cmd,
	}
	if owner != nil {
		e.key = owner.key
	}
	if err := attachBg(e); err != nil {
		// Best-effort: a kill still falls back to a tree kill by pid; only
		// the die-with-lyond guarantee degrades.
		_ = err
	}
	bgMu.Lock()
	bgByID[id] = e
	bgMu.Unlock()

	go reapBackground(e, owner)

	short := command
	if len(short) > 160 {
		short = short[:160] + "…"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "background task #%d started (pid %d)\ncommand: %s\nlog: %s\n",
		id, cmd.Process.Pid, short, logPath)
	b.WriteString("No wall clock — it runs until it exits. Completion arrives as a " +
		"background_end chat event and a system note carrying the log tail; " +
		"read_file the log for the full stream.\n")
	return TextResult(b.String()), nil
}

// reapBackground waits out the process, unregisters it (handle release
// under bgMu, same serialization as KillBackgroundOwner), then reports
// the outcome exactly once.
func reapBackground(e *bgEntry, owner *bgOwner) {
	runErr := e.proc.Wait() // file-backed output: no pipe-holding grandchildren lag here

	code, errText := 0, ""
	switch t := runErr.(type) {
	case nil:
	case *exec.ExitError:
		code = t.ExitCode()
	default:
		errText = runErr.Error()
	}

	bgMu.Lock()
	delete(bgByID, e.id)
	releaseBg(e) // Windows: CloseHandle kills stragglers — under the lock
	bgMu.Unlock()

	if owner == nil || owner.onEnd == nil {
		return
	}
	owner.onEnd(BgInfo{
		ID:        e.id,
		PID:       e.pid,
		Log:       e.log,
		Command:   e.command,
		Code:      code,
		Err:       errText,
		ElapsedMS: time.Since(e.started).Milliseconds(),
		Tail:      readTail(e.log, bgTailLimit),
	})
}

// readTail returns at most n bytes from the end of path, with a marker
// when earlier output was skipped.
func readTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	var buf []byte
	if st.Size() > int64(n) {
		if _, err := f.Seek(st.Size()-int64(n), io.SeekStart); err != nil {
			return ""
		}
		buf = make([]byte, n)
	} else {
		buf = make([]byte, st.Size())
	}
	m, _ := io.ReadFull(f, buf)
	out := strings.TrimSpace(string(buf[:m]))
	if st.Size() > int64(n) {
		out = "…(earlier output omitted)\n" + out
	}
	return out
}

// KillBackgroundOwner tears down every background task started under key
// — session close, prompt interrupt. Idempotent; the waiters do the
// unregistering, so this only has to signal.
func KillBackgroundOwner(key string) {
	if key == "" {
		return
	}
	bgMu.Lock()
	defer bgMu.Unlock()
	for _, e := range bgByID {
		if e.key == key {
			killBgTree(e)
		}
	}
}

// KillAllBackground tears down every live background task — engine
// shutdown. No-op when the registry is already clean.
func KillAllBackground() {
	bgMu.Lock()
	defer bgMu.Unlock()
	for _, e := range bgByID {
		killBgTree(e)
	}
}
