package main

// Tests for the guaranteed-completion post-mortem (tui.go): localDiagnosis
// mines checkable conditions from the run's own record, and
// diagnoseWithFallback runs the bounded retry loop then falls back — the
// diagnose step always finishes with conditions, even when the gateway
// answers every call with a 400 (the messages[N].role incident shape).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lyon-agent/pkg/agent"
)

// TestLocalDiagnosisConditions: the fallback post-mortem turns a failure
// line, the failed steps and the diagnoser's own fate into single-line
// checkable conditions — never an empty result, never a multi-line entry
// (the panel renders one condition per line).
func TestLocalDiagnosisConditions(t *testing.T) {
	t.Run("failure line, failed steps, unavailable diagnoser", func(t *testing.T) {
		tail := []execStep{
			{n: 3, name: "run_shell", status: stepRunning},
			{n: 4, name: "read_file", status: stepDone},
			{n: 5, name: "run_shell", status: stepError, err: "exit status 1"},
			{n: 6, name: "view_image", status: stepError, detail: "bad png header"},
			{n: 7, name: "write_file", status: stepDone},
		}
		d := localDiagnosis("prompt ended: context deadline exceeded\nwhile draining events",
			tail, errTest("gateway 400: messages[69].role"))

		want := []string{
			"run failed: prompt ended: context deadline exceeded while draining events",
			"step #5 run_shell failed: exit status 1",
			"step #6 view_image failed: bad png header",
			"diagnoser model unavailable: gateway 400: messages[69].role",
		}
		if len(d.FailedConditions) != len(want) {
			t.Fatalf("got %d conditions, want %d:\n%s",
				len(d.FailedConditions), len(want), strings.Join(d.FailedConditions, "\n"))
		}
		for i, w := range want {
			if d.FailedConditions[i] != w {
				t.Errorf("condition %d = %q, want %q", i, d.FailedConditions[i], w)
			}
			if strings.Contains(d.FailedConditions[i], "\n") {
				t.Errorf("condition %d spans lines: %q", i, d.FailedConditions[i])
			}
		}
	})

	t.Run("nothing recorded at all", func(t *testing.T) {
		d := localDiagnosis("", nil, nil)
		if len(d.FailedConditions) != 2 {
			t.Fatalf("got %d conditions: %q", len(d.FailedConditions), d.FailedConditions)
		}
		if d.FailedConditions[0] != "run failed with no error line recorded" {
			t.Errorf("first condition = %q", d.FailedConditions[0])
		}
		if !strings.Contains(d.FailedConditions[1], "local post-mortem used") {
			t.Errorf("marker missing: %q", d.FailedConditions[1])
		}
	})

	t.Run("diagnoser succeeded but parsed empty", func(t *testing.T) {
		d := localDiagnosis("boom", nil, nil)
		if len(d.FailedConditions) != 2 ||
			d.FailedConditions[0] != "run failed: boom" ||
			!strings.Contains(d.FailedConditions[1], "diagnoser returned no conditions") {
			t.Fatalf("conditions = %q", d.FailedConditions)
		}
	})
}

// TestDiagnoseWithFallbackCompletes: a gateway that answers every request
// with a citable 400 must not abort the diagnose step — the loop runs its
// bounded attempts (exactly diagnoseAttempts requests reach the wire), then
// localDiagnosis closes it with checkable conditions.
func TestDiagnoseWithFallbackCompletes(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// The incident shape: citable role error, but the repair cannot
		// change a healthy first message — the guard must not loop.
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages[0].role is required"}}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "diagnose-fallback-test-token")

	eng := agent.NewEngine(agent.Config{Workspace: tempDir(t)}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	tail := []execStep{
		{n: 2, name: "run_shell", status: stepError, err: "exit status 1"},
	}
	d := diagnoseWithFallback(sess, "the trajectory", "prompt ended: 400 bad request", tail)

	if n := calls.Load(); n != diagnoseAttempts {
		t.Errorf("wire saw %d requests, want exactly diagnoseAttempts=%d", n, diagnoseAttempts)
	}
	if len(d.FailedConditions) == 0 {
		t.Fatal("diagnose completed without conditions")
	}
	joined := strings.Join(d.FailedConditions, "\n")
	for _, want := range []string{
		"run failed: prompt ended: 400 bad request",
		"step #2 run_shell failed: exit status 1",
		"diagnoser model unavailable:",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("conditions missing %q:\n%s", want, joined)
		}
	}
	for i, c := range d.FailedConditions {
		if strings.Contains(c, "\n") {
			t.Errorf("condition %d spans lines: %q", i, c)
		}
	}
}

// TestDiagnoseWithFallbackLocalMarker: when the model answers but parses to
// nothing, the fallback still completes with the local-marker condition.
func TestDiagnoseWithFallbackLocalMarker(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens is too small"}}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "diagnose-fallback-test-token")

	eng := agent.NewEngine(agent.Config{Workspace: tempDir(t)}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	d := diagnoseWithFallback(sess, "traj", "prompt failed", nil)
	if n := calls.Load(); n != diagnoseAttempts {
		t.Errorf("wire saw %d requests, want %d", n, diagnoseAttempts)
	}
	joined := strings.Join(d.FailedConditions, "\n")
	if !strings.Contains(joined, "run failed: prompt failed") ||
		!strings.Contains(joined, "diagnoser model unavailable") {
		t.Fatalf("conditions incomplete:\n%s", joined)
	}
}
