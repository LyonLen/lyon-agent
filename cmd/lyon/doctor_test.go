package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// capturePipe runs f with *stream (typically &os.Stdout) swapped for a
// pipe and returns what it printed (ANSI intact — these tests assert on
// ESC bytes).
func capturePipe(t *testing.T, stream **os.File, f func()) string {
	t.Helper()
	old := *stream
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*stream = w
	defer func() { *stream = old }()
	f()
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stubGateway points LYON_BASE_URL at a local model list and clears the
// ANTHROPIC_* fallbacks so ambient Claude Code env cannot redirect the
// probe (#54 precedence: LYON_* wins, but empty LYON_* would fall through).
func stubGateway(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LYON_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("LYON_API_KEY", "doctor-test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("LYON_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
}

// doctorScratch isolates LYON_HOME and the workspace so no check reads
// the developer's real ~/.lyon or repository.
func doctorScratch(t *testing.T) (ws string) {
	t.Helper()
	t.Setenv("LYON_HOME", t.TempDir())
	return t.TempDir()
}

// lyondOKForTest reports whether the ambient 8080 will not fail the
// doctor run: absent (skip) or answering /healthz 200. Anything else —
// a foreign service squatting the port — skips the test rather than
// making it machine-dependent.
func lyondOKForTest(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:8080", 300*time.Millisecond)
	if err != nil {
		return // not running → the check skips
	}
	conn.Close()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://127.0.0.1:8080/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		t.Skip("an unhealthy service listens on 8080 — lyond check would fail here")
	}
	resp.Body.Close()
}

// TestCmdDoctorExit0: a healthy gateway plus clean scratch roots → exit
// 0, with the header, per-check lines and summary in the output.
func TestCmdDoctorExit0(t *testing.T) {
	ws := doctorScratch(t)
	stubGateway(t, http.StatusOK, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	lyondOKForTest(t)

	var code int
	out := capturePipe(t, &os.Stdout, func() {
		code = cmdDoctor([]string{"-workspace", ws, "-plain"})
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	for _, want := range []string{
		"lyon doctor — ",
		"✓ gateway",
		"2 models",
		"checks:",
		"0 fail",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✗") {
		t.Errorf("exit 0 must print no ✗:\n%s", out)
	}
	// lyond: either absent (skip) or the ambient daemon answering 200 —
	// lyondOKForTest already ruled out the unhealthy-listener case.
	if !strings.Contains(out, "not running (skip)") && !strings.Contains(out, "healthy") {
		t.Errorf("output missing lyond status:\n%s", out)
	}
}

// TestCmdDoctorExit1: a 500 gateway is a hard failure → exit 1 and the
// report carries a fix line for it.
func TestCmdDoctorExit1(t *testing.T) {
	ws := doctorScratch(t)
	stubGateway(t, http.StatusInternalServerError, "boom")

	var code int
	out := capturePipe(t, &os.Stdout, func() {
		code = cmdDoctor([]string{"-workspace", ws, "-plain"})
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "✗ gateway") {
		t.Errorf("output missing ✗ gateway:\n%s", out)
	}
	if !strings.Contains(out, "fix:") {
		t.Errorf("output missing fix line:\n%s", out)
	}
	if !strings.Contains(out, "1 fail") {
		t.Errorf("summary must say 1 fail:\n%s", out)
	}
}

// TestCmdDoctorExit2: usage errors — unknown flag and stray positional
// argument — both exit 2 without running any check.
func TestCmdDoctorExit2(t *testing.T) {
	for _, args := range [][]string{
		{"-no-such-flag"},
		{"stray-argument"},
	} {
		var code int
		errOut := capturePipe(t, &os.Stderr, func() {
			code = cmdDoctor(args)
		})
		if code != 2 {
			t.Errorf("cmdDoctor(%v) exit = %d, want 2", args, code)
		}
		if !strings.Contains(errOut, "usage: lyon doctor") {
			t.Errorf("cmdDoctor(%v) stderr missing usage:\n%s", args, errOut)
		}
	}
}

// TestCmdDoctorPlainNoESC: -plain must emit zero ESC bytes even when
// color is otherwise on; without it the report is colored.
func TestCmdDoctorPlainNoESC(t *testing.T) {
	ws := doctorScratch(t)
	stubGateway(t, http.StatusInternalServerError, "boom") // guarantees a colored ✗/fix
	oldColor := color
	color = true // package init may have seen NO_COLOR; force color on
	defer func() { color = oldColor }()

	plain := capturePipe(t, &os.Stdout, func() {
		if code := cmdDoctor([]string{"-workspace", ws, "-plain"}); code != 1 {
			t.Errorf("plain exit = %d, want 1", code)
		}
	})
	if strings.Contains(plain, "\x1b") {
		t.Errorf("-plain output contains ESC bytes: %q", plain)
	}
	if !strings.Contains(plain, "✗") {
		t.Errorf("-plain output missing ✗: %q", plain)
	}

	colored := capturePipe(t, &os.Stdout, func() {
		cmdDoctor([]string{"-workspace", ws})
	})
	if !strings.Contains(colored, "\x1b") {
		t.Errorf("colored output has no ESC bytes: %q", colored)
	}
}

// TestUsageMentionsDoctor: both language variants of the top-level usage
// must advertise the new command.
func TestUsageMentionsDoctor(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			oldLang := uiLang
			uiLang = lang
			defer func() { uiLang = oldLang }()
			out := capturePipe(t, &os.Stderr, usage)
			if !strings.Contains(out, "lyon doctor") {
				t.Errorf("%s usage missing \"lyon doctor\":\n%s", lang, out)
			}
		})
	}
}
