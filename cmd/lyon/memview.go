package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/memory"
)

// /memory's rendering and actions, shared by the TUI and the plain REPL.
// Rows are numbered exactly as displayed; the TUI keeps the last render's
// rows in memRows so "/memory N" addresses what the user is looking at.

// rememberDone carries an async /remember result back into the UI loop.
// step is the panel row the run opened (steps.go), resolved on arrival.
// rep carries the distill report (fact names, hot-tier promotions) for
// the echo line; senders holding only a count leave it zero.
type rememberDone struct {
	n    int
	rep  agent.DistillReport
	err  error
	step int
}

// echoLine renders the one-line /remember summary for the command
// panel: the report's fact names and promotions when the pass produced
// them, else the original count-only wording.
func (r rememberDone) echoLine() string {
	if len(r.rep.Names) == 0 {
		return tr(fmt.Sprintf("%s saved %d fact(s) — /memory to review",
			paint(cCyan, "🧠 memory"), r.n),
			fmt.Sprintf("%s 已存 %d 条事实 — /memory 查看",
				paint(cCyan, "🧠 memory"), r.n))
	}
	return paint(cCyan, "🧠 ") + tr(r.rep.EchoLine(), r.rep.EchoLineZH())
}

// memSyncDone carries an async /memory sync result back into the loop.
type memSyncDone struct {
	pushed, pulled int
	err            error
	step           int
}

// memoryTable renders the store as display lines plus the numbered rows
// those lines correspond to. Open never fails; a broken layer just shows
// up empty.
func memoryTable(workspace string) (lines []string, rows []memory.Fact) {
	st := memory.Open(workspace)
	rows = st.List("")
	stats := st.StoreStats()
	syncNote := "off — set LYON_SYNC_URL"
	if os.Getenv("LYON_SYNC_URL") != "" {
		syncNote = "ready"
	}
	lines = append(lines, sanitizeText(fmt.Sprintf(
		"memory  team=%d personal=%d project=%d hot=%d  sync:%s",
		stats.Team, stats.Personal, stats.Project, stats.Hot, syncNote)))
	if len(rows) == 0 {
		lines = append(lines, sanitizeText("  (empty — save_memory from the model, /remember to distill this session)"))
	}
	for i, f := range rows {
		mark := "  "
		if f.Hot {
			mark = "• "
		}
		name := clipDisplay(f.Name, 30)
		desc := clipDisplay(oneLineText(f.Description), 44)
		lines = append(lines, sanitizeText(fmt.Sprintf("  %2d. %s%-9s %-30s hits=%-3d %s  %s",
			i+1, mark, f.Scope, name, f.Hits, f.Updated.Local().Format("01-02"), desc)))
	}
	lines = append(lines,
		sanitizeText("  /memory N  toggle hot tier · /memory forget N  delete · /memory sync|reload"))
	return lines, rows
}

// printMemoryTable is the plain-REPL rendering.
func printMemoryTable(w io.Writer, workspace string) {
	lines, _ := memoryTable(workspace)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// clipDisplay truncates to n runes with an ellipsis (rune-safe for CJK).
func clipDisplay(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// oneLineText flattens whitespace so a fact stays on its table row.
func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// parseRowArg reads the integer in a /memory subcommand tail.
func parseRowArg(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// syncMemory pushes/pulls team+personal with the configured cloud.
func syncMemory(ctx context.Context, workspace string) (int, int, error) {
	return memory.Open(workspace).Sync(ctx)
}

// handlePlainMemory is the line-REPL /memory: same table and subcommands
// as the TUI, printed to w.
func handlePlainMemory(w io.Writer, workspace, args string) {
	st := memory.Open(workspace)
	args = strings.TrimSpace(args)
	table := func(extra string) {
		if extra != "" {
			fmt.Fprintln(w, extra)
		}
		printMemoryTable(w, workspace)
	}
	switch {
	case args == "":
		table("")
	case args == "sync":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pushed, pulled, err := syncMemory(ctx, workspace)
		if err != nil {
			fmt.Fprintln(w, paint(cYellow, "memory sync: "+err.Error()))
			return
		}
		table(fmt.Sprintf("%s sync: pushed %d, pulled %d", paint(cCyan, "🧠 memory"), pushed, pulled))
	case args == "reload":
		st.Reload()
		table(paint(cDim, "memory reloaded from disk"))
	case strings.HasPrefix(args, "forget"):
		_, rows := memoryTable(workspace)
		idx, ok := parseRowArg(strings.TrimPrefix(args, "forget"))
		if !ok || idx > len(rows) {
			fmt.Fprintln(w, paint(cYellow, "usage: /memory forget N — N comes from the table"))
			return
		}
		f := rows[idx-1]
		if err := st.Delete(f.Scope, f.Name); err != nil {
			fmt.Fprintln(w, paint(cYellow, "forget: "+err.Error()))
			return
		}
		table(paint(cDim, fmt.Sprintf("forgot (%s) %s", f.Scope, f.Name)))
	default:
		_, rows := memoryTable(workspace)
		idx, ok := parseRowArg(args)
		if !ok || idx > len(rows) {
			fmt.Fprintln(w, paint(cYellow, "usage: /memory [N | forget N | sync | reload]"))
			return
		}
		f := rows[idx-1]
		hot, err := st.ToggleHot(f.Scope, f.Name)
		if err != nil {
			fmt.Fprintln(w, paint(cYellow, "hot: "+err.Error()))
			return
		}
		state := "cold"
		if hot {
			state = "hot"
		}
		table(paint(cDim, fmt.Sprintf("(%s) %s is now %s", f.Scope, f.Name, state)))
	}
}
