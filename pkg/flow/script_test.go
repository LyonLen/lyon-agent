package flow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freezeFixture isolates $LYON_HOME and returns a two-step script whose
// steps echo a substituted arg — no build system, no network, so the test is
// about the fixation mechanics, not the host toolchain.
func freezeFixture(t *testing.T) Script {
	t.Helper()
	t.Setenv("LYON_HOME", t.TempDir())
	s := Script{
		Slug:        "greet",
		Description: "say hello to a name",
		Args:        []string{"name"},
		Steps:       []string{"echo hello-{{name}}", "echo bye-{{name}}"},
		Runs:        2,
	}
	if err := FreezeScript(s); err != nil {
		t.Fatalf("FreezeScript: %v", err)
	}
	return s
}

// TestFreezeAndRunScript: a frozen script lands as both a JSON manifest and
// a runnable .sh, runs its steps in order with args substituted, and counts
// the completed green run back into the manifest.
func TestFreezeAndRunScript(t *testing.T) {
	s := freezeFixture(t)

	// Both artifacts exist on disk.
	man := filepath.Join(FlowsDir(), "greet.json")
	sh := filepath.Join(FlowsDir(), "greet.sh")
	if _, err := os.Stat(man); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	raw, err := os.ReadFile(sh)
	if err != nil {
		t.Fatalf("script missing: %v", err)
	}
	if !strings.Contains(string(raw), "echo hello-$LYON_ARG_NAME") {
		t.Errorf("script did not substitute arg into a positional:\n%s", raw)
	}

	// Load returns what was frozen.
	got, err := LoadScript("greet")
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	if got.Slug != s.Slug || len(got.Steps) != 2 || got.Runs != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Run executes both steps in order, substituting the arg.
	res, err := RunScript(context.Background(), "greet", map[string]string{"name": "lyon"}, "")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if !res.OK {
		t.Fatalf("run not OK: %+v", res)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(res.Steps))
	}
	if !strings.Contains(res.Steps[0].Output, "hello-lyon") {
		t.Errorf("step 0 output = %q, want hello-lyon", res.Steps[0].Output)
	}
	if !strings.Contains(res.Steps[1].Output, "bye-lyon") {
		t.Errorf("step 1 output = %q, want bye-lyon", res.Steps[1].Output)
	}

	// A completed green run bumped the manifest's counter.
	after, err := LoadScript("greet")
	if err != nil {
		t.Fatalf("LoadScript after run: %v", err)
	}
	if after.Runs != 3 {
		t.Errorf("Runs after one green = %d, want 3", after.Runs)
	}
}

// TestRunScriptMissingArg: a declared arg with no supplied value fails
// BEFORE any step runs — a half-substituted command must never execute.
func TestRunScriptMissingArg(t *testing.T) {
	freezeFixture(t)
	res, err := RunScript(context.Background(), "greet", map[string]string{}, "")
	if err == nil {
		t.Fatalf("missing arg should error, got %+v", res)
	}
	if len(res.Steps) != 0 {
		t.Errorf("no step should have run, ran %d", len(res.Steps))
	}
}

// TestRunScriptUnsubstitutedPlaceholder: a step carrying a placeholder that
// is not a declared arg refuses the whole run BEFORE any step executes — a
// half-substituted chain must never run, not even its early steps.
func TestRunScriptUnsubstitutedPlaceholder(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	if err := FreezeScript(Script{
		Slug:  "bad",
		Steps: []string{"echo ok", "echo {{missing}}"},
		Args:  nil,
	}); err != nil {
		t.Fatalf("FreezeScript: %v", err)
	}
	res, err := RunScript(context.Background(), "bad", map[string]string{}, "")
	if err == nil {
		t.Fatalf("unsubstituted placeholder should error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "unsubstituted") {
		t.Errorf("err = %v, want an unsubstituted-placeholder verdict", err)
	}
	if len(res.Steps) != 0 {
		t.Errorf("no step should have run, ran %d", len(res.Steps))
	}
}

// TestRunScriptStopsAtFirstFailure: a chain is only validated up to its
// first broken link — steps after a failing one never execute.
func TestRunScriptStopsAtFirstFailure(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "should-not-exist")
	if err := FreezeScript(Script{
		Slug:  "chain",
		Steps: []string{"exit 1", "echo " + marker + " > " + marker},
	}); err != nil {
		t.Fatalf("FreezeScript: %v", err)
	}
	res, err := RunScript(context.Background(), "chain", map[string]string{}, "")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if res.OK {
		t.Fatalf("chain should fail: %+v", res)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %d, want 1 (stopped at the failing step)", len(res.Steps))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("step after a failing link executed — the chain did not stop")
	}
}

// TestFreezeScriptRejectsBadInput: slug hygiene and empty steps are refused,
// so a bad freeze can never shadow a real script or land a no-op.
func TestFreezeScriptRejectsBadInput(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	if err := FreezeScript(Script{Slug: "../evil", Steps: []string{"echo x"}}); err == nil {
		t.Error("a path-hostile slug should be refused")
	}
	if err := FreezeScript(Script{Slug: "empty", Steps: nil}); err == nil {
		t.Error("a freeze with no steps should be refused")
	}
	if err := FreezeScript(Script{Slug: "blank", Steps: []string{"   "}}); err == nil {
		t.Error("a blank step should be refused")
	}
}

// TestFreezeScriptKeepsHigherEvidence: re-freezing a slug must not regress
// its recorded green-run evidence to a thinner count.
func TestFreezeScriptKeepsHigherEvidence(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	if err := FreezeScript(Script{Slug: "ev", Steps: []string{"echo x"}, Runs: 5}); err != nil {
		t.Fatalf("FreezeScript: %v", err)
	}
	if err := FreezeScript(Script{Slug: "ev", Steps: []string{"echo y"}, Runs: 1}); err != nil {
		t.Fatalf("FreezeScript: %v", err)
	}
	got, err := LoadScript("ev")
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	if got.Runs != 5 {
		t.Errorf("Runs = %d, want the higher evidence 5 preserved", got.Runs)
	}
	// The newer steps did land.
	if got.Steps[0] != "echo y" {
		t.Errorf("steps = %v, want the re-frozen step", got.Steps)
	}
}
