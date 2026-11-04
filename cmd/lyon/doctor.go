// `lyon doctor`: one-shot health check whose output is meant to be
// pasted straight into an issue. Flags follow timercli's conventions —
// 0 = no ✗ (warns don't fail), 1 = at least one ✗, 2 = usage error.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"lyon-agent/pkg/doctor"
)

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	workspace := fs.String("workspace", "", "workspace root to inspect (default: current directory)")
	plain := fs.Bool("plain", false, "no color, no ESC bytes (NO_COLOR does the same)")
	timeout := fs.Duration("timeout", 0, "per-check network timeout override (gateway 8s, mcp 15s, lyond 2s)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: lyon doctor [-workspace dir] [-plain] [-timeout duration]")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "doctor: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}

	ws := strings.TrimSpace(*workspace)
	if ws == "" {
		if wd, err := os.Getwd(); err == nil {
			ws = wd
		}
	}
	if abs, err := filepath.Abs(ws); err == nil {
		ws = abs
	}

	// Color on unless NO_COLOR or -plain — paint() alone cannot see -plain.
	useColor := color && !*plain
	paintIf := func(c, s string) string {
		if !useColor || s == "" {
			return s
		}
		return c + s + cReset
	}

	fmt.Println(paintIf(cBold, "lyon doctor") + paintIf(cDim, fmt.Sprintf(
		" — %s · %s · %s", time.Now().Format("2006-01-02"), ws, runtime.Version())))

	checks := doctor.Run(context.Background(), doctor.Options{
		Workspace: ws,
		Timeout:   *timeout,
	})

	nameWidth := 0
	for _, c := range checks {
		if len(c.Name) > nameWidth {
			nameWidth = len(c.Name)
		}
	}
	ok, fail, warn := 0, 0, 0
	for _, c := range checks {
		var sym, col string
		switch c.Status {
		case doctor.StatusOK:
			ok, sym, col = ok+1, "✓", cGreen
		case doctor.StatusWarn:
			warn, sym, col = warn+1, "⚠", cYellow
		default:
			fail, sym, col = fail+1, "✗", cRed
		}
		fmt.Printf("%s %-*s  %s\n", paintIf(col, sym), nameWidth, c.Name,
			paintIf(cDim, c.Detail))
		if c.Status != doctor.StatusOK && c.Fix != "" {
			fmt.Printf("    %s\n", paintIf(cDim, "fix: "+c.Fix))
		}
	}

	fmt.Printf("%s\n", paintIf(cBold, fmt.Sprintf("%d checks: %d ok, %d fail, %d warn",
		len(checks), ok, fail, warn)))
	fmt.Printf("%s\n", paintIf(cDim, tr(
		"full log at "+doctor.LogPath(),
		"完整日志在 "+doctor.LogPath())))

	if fail > 0 {
		return 1
	}
	return 0
}
