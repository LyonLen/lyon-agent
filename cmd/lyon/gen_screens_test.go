package main

// Screenshot generator for the docs site: renders real TUI frames
// (View()) and converts their ANSI bytes into self-contained HTML
// terminal pages under docs/screens/. Gated so normal test runs skip it:
//
//	LYON_GEN_SCREENS=1 go test ./cmd/lyon -run TestGenerateScreens -count=1

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
)

// ── ANSI → HTML ───────────────────────────────────────────────────

// xterm256 converts a 0–255 palette index to a CSS hex color: the 16
// system colors, the 6×6×6 cube, then the grayscale ramp.
func xterm256(n int) string {
	base := [16]string{
		"#000000", "#cd3131", "#0dbc79", "#e5e510",
		"#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
		"#666666", "#f14c4c", "#23d18b", "#f5f543",
		"#3b8eea", "#d670d6", "#29b8db", "#ffffff",
	}
	switch {
	case n < 16:
		return base[n]
	case n < 232:
		v := n - 16
		levels := []int{0, 95, 135, 175, 215, 255}
		return fmt.Sprintf("#%02x%02x%02x",
			levels[v/36], levels[(v/6)%6], levels[v%6])
	default:
		g := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	}
}

// ansiSeg is one run of text with a resolved style.
type ansiSeg struct {
	css  string // "" = default
	text string
}

// splitANSI parses SGR runs out of a rendered frame: basic 16 colors,
// bold/dim, xterm-256 (38;5;n) and truecolor (38;2;r;g;b). Every other
// CSI sequence (cursor moves, clears) is dropped — View frames are
// print-style output.
func splitANSI(s string) []ansiSeg {
	var segs []ansiSeg
	var css, buf strings.Builder
	fg := "" // current fg color css

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		segs = append(segs, ansiSeg{css: css.String(), text: buf.String()})
		buf.Reset()
	}
	apply := func(params string) {
		if params == "" {
			params = "0"
		}
		fields := strings.Split(params, ";")
		for i := 0; i < len(fields); i++ {
			n := -1
			fmt.Sscanf(fields[i], "%d", &n)
			switch {
			case n <= 0:
				fg = ""
				css.Reset()
			case n == 1:
				css.WriteString("font-weight:700;")
			case n == 2:
				css.WriteString("opacity:.65;")
			case n == 22:
				css.Reset()
				if fg != "" {
					fmt.Fprintf(&css, "color:%s;", fg)
				}
			case n >= 30 && n <= 37:
				fg = xterm256(n - 30)
				css.Reset()
				fmt.Fprintf(&css, "color:%s;", fg)
			case n >= 90 && n <= 97:
				fg = xterm256(n - 90 + 8)
				css.Reset()
				fmt.Fprintf(&css, "color:%s;", fg)
			case n == 39:
				fg = ""
				css.Reset()
			case n == 38 && i+1 < len(fields):
				mode, _ := strconv.Atoi(fields[i+1])
				switch mode {
				case 5:
					idx, _ := strconv.Atoi(fields[i+2])
					fg = xterm256(idx)
					css.Reset()
					fmt.Fprintf(&css, "color:%s;", fg)
					i += 2
				case 2:
					r, _ := strconv.Atoi(fields[i+2])
					g, _ := strconv.Atoi(fields[i+3])
					b, _ := strconv.Atoi(fields[i+4])
					fg = fmt.Sprintf("#%02x%02x%02x", r, g, b)
					css.Reset()
					fmt.Fprintf(&css, "color:%s;", fg)
					i += 4
				}
			}
		}
	}

	r := []rune(s)
	for i := 0; i < len(r); i++ {
		if r[i] != '\x1b' {
			buf.WriteRune(r[i])
			continue
		}
		if i+1 >= len(r) {
			break
		}
		if r[i+1] != '[' { // stray ESC + char: drop both
			i++
			continue
		}
		j := i + 2
		for j < len(r) && !((r[j] >= 'A' && r[j] <= 'Z') || (r[j] >= 'a' && r[j] <= 'z')) {
			j++
		}
		if j >= len(r) {
			break
		}
		if r[j] == 'm' {
			flush()
			apply(string(r[i+2 : j]))
		}
		// Non-SGR CSI (cursor/erase): the buffered text before it is
		// already queued; the sequence itself disappears.
		i = j
	}
	flush()
	return segs
}

// ansiToHTML renders split segments as spans with HTML-escaped text.
func ansiToHTML(s string) string {
	var b strings.Builder
	for _, seg := range splitANSI(s) {
		text := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(seg.text)
		if seg.css == "" {
			b.WriteString(text)
			continue
		}
		fmt.Fprintf(&b, `<span style="%s">%s</span>`, seg.css, text)
	}
	return b.String()
}

// shotPage wraps a converted frame in a self-contained terminal page —
// the docs site iframes these directly.
func shotPage(title, frame string) string {
	return `<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + title + `</title>
<style>
  :root { color-scheme: dark; }
  html, body { margin:0; background:#0a0e14; }
  body { padding:14px; font-family: "Cascadia Code","Consolas",ui-monospace,monospace; }
  .bar { display:flex; gap:7px; padding:0 4px 10px; }
  .dot { width:11px; height:11px; border-radius:50%; }
  .r{background:#ff5f57}.y{background:#febc2e}.g{background:#28c840}
  pre { margin:0; padding:16px 18px; background:#0d1117; border:1px solid #21262d;
        border-radius:10px; font-size:12.5px; line-height:1.42;
        color:#c9d1d9; overflow-x:auto; }
</style></head>
<body>
<div class="bar"><span class="dot r"></span><span class="dot y"></span><span class="dot g"></span></div>
<pre>` + ansiToHTML(frame) + `</pre>
</body></html>`
}

// ── scenes ────────────────────────────────────────────────────────

// shotModel builds a TUI at docs-friendly size.
func shotModel(t *testing.T) *tuiModel {
	t.Helper()
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
	m.cfg.Model = "claude-opus-5-5"
	m.cfg.SubagentModel = "claude-haiku-5-5"
	return m
}

// writeShot converts one frame and writes its page under docs/screens.
func writeShot(t *testing.T, name, title string, m *tuiModel) {
	t.Helper()
	// go test runs with the package dir as cwd — climb out to the repo root.
	dir := filepath.Join("..", "..", "docs", "screens")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	page := shotPage(title, m.View())
	if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "<pre>") || len(page) < 500 {
		t.Fatalf("%s: suspiciously empty shot (%d bytes)", name, len(page))
	}
}

// TestGenerateScreens renders the docs screenshots. Skipped unless
// LYON_GEN_SCREENS=1 — it writes into docs/, not into test state.
func TestGenerateScreens(t *testing.T) {
	if os.Getenv("LYON_GEN_SCREENS") != "1" {
		t.Skip("set LYON_GEN_SCREENS=1 to regenerate docs/screens")
	}
	t.Setenv("LYON_HOME", t.TempDir())

	// 1. The workbench: a real multi-step run with the audit panel,
	//    agent badges, a subagent and a memory injection.
	workbench := shotModel(t)
	workbench.startPromptStep("把 pkg/eval 的测试跑一遍，修掉失败项，然后总结改动")
	workbench.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"go test ./pkg/eval/ -count=1"}`),
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 1, ToolName: "run_shell",
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventToolExecEnd, ExecID: 1, ToolName: "run_shell",
		Detail: "exit_code: 1 — 2 tests failed",
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t2", ToolName: "edit_file",
		Input: json.RawMessage(`{"path":"pkg/eval/score_test.go"}`),
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventToolExecEnd, ExecID: 2, ToolName: "edit_file",
		Detail: "3 insertions, 1 deletion",
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t3", ToolName: "run_subagent",
		Input: json.RawMessage(`{"task":"查一下评分权重的设计依据"}`),
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventSubagentEnd, ToolName: "claude-haiku-5-5",
		Detail: "3 API calls", DurationMS: 1420,
	})
	workbench.applyEvent(agent.Event{
		Type: agent.EventMemoryInjected, Detail: "842 bytes of hot memory",
	})
	workbench.applyEvent(agent.Event{Type: agent.EventTextDelta,
		Text: "两处断言与新的四维评分不一致，已对齐：Correctness 权重改读 WeightCorrectness 常量，快照测试的期望总分同步更新。\n"})
	workbench.applyEvent(agent.Event{Type: agent.EventPromptEnd, Iterations: 6})
	writeShot(t, "tui-workbench", "lyon chat — 工作台", workbench)

	// 2. Routing: the bare evidence table + a classified task.
	route := shotModel(t)
	// Seed a believable ledger so the table and exam clock show data.
	lb := eval.OpenLeaderboard()
	lb.Record("coding", eval.Scorecard{Agent: "claude-opus-5-5", Total: 0.91, Passed: true})
	lb.Record("coding", eval.Scorecard{Agent: "claude-haiku-5-5", Total: 0.78, Passed: true})
	for i := 0; i < 6; i++ {
		lb.CountRound("coding")
	}
	rt := routing.Open()
	rt.Record("coding", "claude-opus-5-5", "claude-opus-5-5", 0.91)
	rt.Record("fix", "claude-haiku-5-5", "claude-haiku-5-5", 0.84)
	route.showRoute("")
	route.showRoute("implement a dark-mode export feature for the report page")
	writeShot(t, "tui-route", "lyon chat — /route 路由推荐", route)

	// 3. Workflow browser: list + the feature-dev node walk.
	flowM := shotModel(t)
	flowM.flowCommand("list")
	flowM.flowCommand("show feature-dev")
	writeShot(t, "tui-flow", "lyon chat — /flow workflow", flowM)

	// 4. Exam: the async row in the panel + the graded report below.
	examM := shotModel(t)
	examM.startPromptStep("review the session restore path")
	step := examM.pushAsyncStep(stepEval, "exam.review", "manual — closed-book…")
	examM.Update(examDone{
		class: "review", why: "manual",
		step: step,
		res: eval.ExamResult{
			Class: "review", Pct: 0.75, Passed: true,
			Answers: []eval.ExamAnswer{
				{Q: "A change adds new branches but no tests for them. What is the usual review verdict?", OK: true},
				{Q: "Name the review question that asks whether error paths are handled.", OK: true},
				{Q: "What should a reviewer do with a nitpick (style-only remark)?", OK: true},
				{Q: "Which is checked first in a review: design or naming?", OK: false},
			},
		},
	})
	writeShot(t, "tui-exam", "lyon chat — /exam 闭卷考核", examM)

	// 5. Help: the command reference as the TUI prints it.
	help := shotModel(t)
	help.slash("/help")
	writeShot(t, "tui-help", "lyon chat — /help 命令参考", help)
}
