package timer

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitLog polls until substr appears in today's timer log or the
// deadline passes.
func waitLog(t *testing.T, home, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		log := readTodayLog(t, home)
		if strings.Contains(log, substr) {
			return log
		}
		time.Sleep(10 * time.Millisecond)
	}
	log := readTodayLog(t, home)
	t.Fatalf("log never contained %q:\n%s", substr, log)
	return log
}

func TestSchedulerFiresIntervalTask(t *testing.T) {
	home := scratchHome(t)
	var fires atomic.Int64
	s, err := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tk := &Task{ID: "tick", Kind: KindInterval, EveryMS: 20, Prompt: "x", Enabled: true}
	if err := s.Add(tk); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for fires.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	if got := fires.Load(); got < 3 {
		t.Fatalf("want >= 3 fires, got %d", got)
	}
	// Bookkeeping must be persisted.
	saved, err := Load("tick")
	if err != nil {
		t.Fatalf("Load after run: %v", err)
	}
	if saved.RunCount < 1 || saved.LastStatus != "ok" || saved.LastRun == nil {
		t.Fatalf("bookkeeping not persisted: %+v", saved)
	}
	waitLog(t, home, "timer tick: ok in", 2*time.Second)
}

func TestSchedulerStopHaltsFires(t *testing.T) {
	scratchHome(t)
	var fires atomic.Int64
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	if err := s.Add(&Task{ID: "tick", Kind: KindInterval, EveryMS: 10, Prompt: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for fires.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	after := fires.Load()
	time.Sleep(100 * time.Millisecond)
	if got := fires.Load(); got != after {
		t.Fatalf("fired after Stop: %d -> %d", after, got)
	}
	// Stop twice must not panic or block.
	s.Stop()
}

func TestSchedulerOverlapSkip(t *testing.T) {
	home := scratchHome(t)
	var started, finished atomic.Int64
	release := make(chan struct{})
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		started.Add(1)
		<-release
		finished.Add(1)
		return nil
	}, time.Second)
	// 15ms interval, runner blocked: ticks pile up while the first
	// run is live and must be skipped, not queued.
	if err := s.Add(&Task{ID: "slow", Kind: KindInterval, EveryMS: 15, Prompt: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	// Wait until the first run is in flight, then let a few ticks pass.
	deadline := time.Now().Add(3 * time.Second)
	for started.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond) // ~5 ticks arrive while blocked
	// Assert while still blocked: no second run may have started.
	if got := started.Load(); got != 1 {
		t.Fatalf("overlap=skip must not start a second run while blocked, started=%d", got)
	}
	close(release)
	deadline = time.Now().Add(3 * time.Second)
	for finished.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	waitLog(t, home, "previous run still going, skipped this tick", 2*time.Second)
}

func TestSchedulerOverlapQueueCoalesces(t *testing.T) {
	home := scratchHome(t)
	var started atomic.Int64
	release := make(chan struct{})
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		if started.Add(1) == 1 {
			<-release // first run blocks; ticks during it coalesce
		}
		return nil
	}, time.Second)
	tk := &Task{ID: "mustrun", Kind: KindInterval, EveryMS: 15, Prompt: "x", Enabled: true, Overlap: OverlapQueue}
	if err := s.Add(tk); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for started.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond) // several ticks arrive while blocked
	// Coalescing proof: ~5 blocked ticks armed exactly ONE catch-up.
	log := readTodayLog(t, home)
	if n := strings.Count(log, "queued one catch-up"); n != 1 {
		t.Fatalf("blocked ticks must coalesce into one catch-up, got %d:\n%s", n, log)
	}
	close(release)
	// The catch-up run must fire after the first finishes.
	deadline = time.Now().Add(3 * time.Second)
	for started.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	if got := started.Load(); got < 2 {
		t.Fatalf("overlap=queue must run a catch-up, started=%d", got)
	}
}

func TestSchedulerRunnerErrorLoggedAndRecorded(t *testing.T) {
	home := scratchHome(t)
	boom := errors.New("boom")
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error { return boom }, time.Second)
	if err := s.Add(&Task{ID: "bad", Kind: KindInterval, EveryMS: 20, Prompt: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	log := waitLog(t, home, "timer bad: error", 3*time.Second)
	if !strings.Contains(log, "boom") {
		t.Fatalf("error text missing from log:\n%s", log)
	}
	s.Stop()
	saved, err := Load("bad")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastStatus != "error" || !strings.Contains(saved.LastError, "boom") {
		t.Fatalf("error not recorded on task: %+v", saved)
	}
}

func TestSchedulerTimeoutRecorded(t *testing.T) {
	home := scratchHome(t)
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		<-ctx.Done() // wedge until the timeout kills us
		return ctx.Err()
	}, time.Second)
	tk := &Task{ID: "wedge", Kind: KindInterval, EveryMS: 10, Prompt: "x", Enabled: true, TimeoutMS: 30}
	if err := s.Add(tk); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitLog(t, home, "timer wedge: timeout", 3*time.Second)
	s.Stop()
	saved, err := Load("wedge")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastStatus != "timeout" {
		t.Fatalf("want status timeout, got %+v", saved)
	}
}

func TestSchedulerOnceFiresAndSpends(t *testing.T) {
	home := scratchHome(t)
	var fires atomic.Int64
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	// 200ms, not 30ms: Add persists the file before scheduling, and
	// under heavy load (parallel test binaries, AV scanning %TEMP%)
	// that write alone can eat 30ms — the one-shot would arrive "in
	// the past" and Add would fail with ErrNoNext.
	tk := &Task{ID: "oneshot", Kind: KindOnce, At: timePtr(time.Now().Add(200 * time.Millisecond)), Prompt: "x", Enabled: true}
	if err := s.Add(tk); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for fires.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond) // long enough for a second fire if broken
	s.Stop()
	if got := fires.Load(); got != 1 {
		t.Fatalf("one-shot fired %d times, want exactly 1", got)
	}
	waitLog(t, home, "schedule spent", 2*time.Second)
	// The final run must persist its bookkeeping and self-disable,
	// even though the id left the heap at fire time.
	saved, err := Load("oneshot")
	if err != nil {
		t.Fatalf("Load after spend: %v", err)
	}
	if saved.RunCount != 1 || saved.LastStatus != "ok" || saved.Enabled {
		t.Fatalf("spent one-shot not persisted as done: %+v", saved)
	}
}

func TestSchedulerDisabledTaskDoesNotFire(t *testing.T) {
	scratchHome(t)
	var fires atomic.Int64
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	tk := okTask("off")
	tk.EveryMS = 10
	tk.Enabled = false
	if err := s.Add(tk); err != nil {
		t.Fatalf("Add disabled: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	time.Sleep(80 * time.Millisecond)
	s.Stop()
	if got := fires.Load(); got != 0 {
		t.Fatalf("disabled task fired %d times", got)
	}
}

func TestSchedulerMissOnceCatchesUpOnce(t *testing.T) {
	home := scratchHome(t)
	// Simulate downtime: a 20ms-interval task whose last run was an
	// hour ago. miss=once must fire exactly one catch-up at startup.
	long := time.Now().Add(-time.Hour)
	tk := &Task{ID: "catchup", Kind: KindInterval, EveryMS: 20, Prompt: "x", Enabled: true,
		Miss: MissOnce, LastRun: &long, RunCount: 40}
	if err := Save(tk); err != nil {
		t.Fatal(err)
	}
	var fires atomic.Int64
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitLog(t, home, "timer catchup: catch-up fire (missed at least one tick while down)", 3*time.Second)
	// Let it settle: the catch-up plus a couple of live ticks is fine;
	// an unbounded replay of the hour's misses is not.
	time.Sleep(120 * time.Millisecond)
	s.Stop()
	if got := fires.Load(); got > 20 {
		t.Fatalf("miss=once replayed the past: %d fires", got)
	}
}

func TestSchedulerMissSkipSchedulesFromNow(t *testing.T) {
	home := scratchHome(t)
	long := time.Now().Add(-time.Hour)
	tk := &Task{ID: "noskip", Kind: KindInterval, EveryMS: 20, Prompt: "x", Enabled: true,
		Miss: MissSkip, LastRun: &long}
	if err := Save(tk); err != nil {
		t.Fatal(err)
	}
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error { return nil }, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readTodayLog(t, home), "timer noskip: fire") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.Stop()
	log := readTodayLog(t, home)
	if strings.Contains(log, "catch-up fire") {
		t.Fatalf("miss=skip must not catch up:\n%s", log)
	}
}

func TestSchedulerRemoveStopsFutureFires(t *testing.T) {
	scratchHome(t)
	var fires atomic.Int64
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	if err := s.Add(&Task{ID: "gone", Kind: KindInterval, EveryMS: 10, Prompt: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for fires.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	after := fires.Load()
	time.Sleep(80 * time.Millisecond)
	s.Stop()
	if got := fires.Load(); got != after {
		t.Fatalf("fired after Remove: %d -> %d", after, got)
	}
	if _, err := Load("gone"); err == nil {
		t.Fatal("file still exists after Remove")
	}
}

func TestSchedulerEnableToggles(t *testing.T) {
	scratchHome(t)
	var fires atomic.Int64
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	tk := okTask("toggle")
	tk.EveryMS = 10
	tk.Enabled = false
	if err := s.Add(tk); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	time.Sleep(40 * time.Millisecond)
	if got := fires.Load(); got != 0 {
		t.Fatalf("disabled fired %d", got)
	}
	if err := s.Enable("toggle", true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for fires.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Enable("toggle", false); err != nil {
		t.Fatal(err)
	}
	pre := fires.Load()
	time.Sleep(60 * time.Millisecond)
	// Stop drains the at-most-one run that was already launched when
	// disable landed (running-flag serialization), making the count
	// stable. A disable that keeps firing would add ~6 fires across
	// the 60ms sleep (10ms interval) — far beyond the tolerance.
	s.Stop()
	if got := fires.Load(); got-pre > 1 {
		t.Fatalf("fired after disable: %d -> %d", pre, got)
	}
	if got := fires.Load(); got == 0 {
		t.Fatal("never fired while enabled")
	}
}

func TestSchedulerStartLoadsPersistedTasks(t *testing.T) {
	scratchHome(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		mu.Lock()
		seen[tk.ID] = true
		mu.Unlock()
		return nil
	}, time.Second)
	for _, id := range []string{"a", "b"} {
		tk := &Task{ID: id, Kind: KindInterval, EveryMS: 15, Prompt: "x", Enabled: true}
		if err := Save(tk); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	mu.Lock()
	defer mu.Unlock()
	if !seen["a"] || !seen["b"] {
		t.Fatalf("persisted tasks not loaded: %v", seen)
	}
}

// TestSchedulerRejectsFastPayloadInterval: the fork-storm guard.
// scratchHome lowered minPayloadEveryMS for the fast-tick fixtures;
// these checks pin the production floor (1000ms) behavior: Add refuses
// to persist a sub-floor payload interval, a hostile on-disk copy is
// disarmed at load (enabled=false + one WARN, so it cannot revive on
// every launch), and Enable refuses to arm it back up.
func TestSchedulerRejectsFastPayloadInterval(t *testing.T) {
	home := scratchHome(t)
	minPayloadEveryMS = 1000 // production floor (scratchHome's cleanup restores)

	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error { return nil }, time.Second)

	// Add path: rejected outright, nothing lands on disk.
	err := s.Add(&Task{ID: "fast", Kind: KindInterval, EveryMS: 15, Prompt: "x", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "below 1000ms") {
		t.Fatalf("Add 15ms payload interval: err = %v, want below-floor rejection", err)
	}
	if _, lerr := Load("fast"); !errors.Is(lerr, os.ErrNotExist) {
		t.Fatalf("rejected task must not persist, Load err = %v", lerr)
	}

	// Load path: a pre-existing hostile enabled=true copy is rejected
	// from the heap and written back disabled (anti-revival).
	hostile := &Task{ID: "hostile", Kind: KindInterval, EveryMS: 15, Prompt: "x", Enabled: true}
	if err := Save(hostile); err != nil {
		t.Fatal(err)
	}
	var fires atomic.Int64
	s2, _ := NewScheduler(func(ctx context.Context, tk *Task) error {
		fires.Add(1)
		return nil
	}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s2.Start(ctx)
	waitLog(t, home, "fork-storm guard", 3*time.Second)
	s2.Stop()
	saved, err := Load("hostile")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Enabled {
		t.Fatalf("hostile task must be persisted disabled, got enabled=true")
	}
	if got := fires.Load(); got != 0 {
		t.Fatalf("sub-floor task fired %d times", got)
	}

	// Enable path: arming it by hand is refused and stays disabled.
	if err := s2.Enable("hostile", true); err == nil || !strings.Contains(err.Error(), "below 1000ms") {
		t.Fatalf("Enable(true) of fast task: err = %v, want rejection", err)
	}
	reloaded, err := Load("hostile")
	if err != nil || reloaded.Enabled {
		t.Fatalf("enable must leave the copy disabled, enabled=%v err=%v", reloaded != nil && reloaded.Enabled, err)
	}
}

// TestSchedulerIntervalFloorBoundary: exactly 1000ms passes, and the
// floor only bites payload-carrying intervals — a payload-less pure
// tick stays free (it forks nothing).
func TestSchedulerIntervalFloorBoundary(t *testing.T) {
	scratchHome(t)
	minPayloadEveryMS = 1000 // production floor (scratchHome's cleanup restores)

	s, _ := NewScheduler(func(ctx context.Context, tk *Task) error { return nil }, time.Second)
	if err := s.Add(&Task{ID: "edge", Kind: KindInterval, EveryMS: 1000, Prompt: "x", Enabled: true}); err != nil {
		t.Fatalf("1000ms boundary must pass: %v", err)
	}
	saved, err := Load("edge")
	if err != nil || !saved.Enabled {
		t.Fatalf("boundary task not stored enabled: enabled=%v err=%v", saved != nil && saved.Enabled, err)
	}
	if err := s.Remove("edge"); err != nil {
		t.Fatal(err)
	}

	// Payload-less interval: exempt at any interval (Validate happens
	// to require exactly one payload today, so this guards the floor
	// itself — a pure tick forks nothing and must never trip it).
	if (&Task{Kind: KindInterval, EveryMS: 15}).tooFast() {
		t.Fatal("payload-less interval must be exempt from the floor")
	}
	// 999ms with a payload: one millisecond under the floor still bites.
	if !(&Task{Kind: KindInterval, EveryMS: 999, Prompt: "x"}).tooFast() {
		t.Fatal("999ms payload interval must be below the floor")
	}
}

func TestNewSchedulerRejectsNilRunner(t *testing.T) {
	if _, err := NewScheduler(nil, time.Second); err == nil {
		t.Fatal("nil runner must be rejected")
	}
}

func timePtr(t time.Time) *time.Time { return &t }
