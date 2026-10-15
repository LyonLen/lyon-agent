package timer

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"time"
)

// Runner executes one fire of a task. It runs in its own goroutine
// with a timeout context — a slow or wedged runner delays only its own
// completion log, never the schedule itself. Errors are logged and
// recorded on the task; they never bubble into the loop. Contract: the
// runner MUST return promptly once ctx is done (Stop cancels it and
// then waits, bounded by a grace period, for in-flight runs).
type Runner func(ctx context.Context, t *Task) error

// entry is one scheduled task inside the heap.
type entry struct {
	id   string
	task *Task
	next time.Time
	idx  int  // heap index, for O(log n) remove/fix; -1 while popped
	dead bool // removed/disabled while the loop held it out of the heap
}

type fireHeap []*entry

func (h fireHeap) Len() int           { return len(h) }
func (h fireHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h fireHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}
func (h *fireHeap) Push(x any) {
	e := x.(*entry)
	e.idx = len(*h)
	*h = append(*h, e)
}
func (h *fireHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.idx = -1
	*h = old[:n-1]
	return e
}

// Scheduler drives all in-process tasks: one goroutine, one min-heap
// of next-fire times, a wake channel for Add/Remove/Enable. Fires run
// through the injected Runner in separate goroutines under a timeout;
// overlap and miss policies are per task (see timer.go, DESIGN.md).
type Scheduler struct {
	runner  Runner
	timeout time.Duration // default per-run timeout; task TimeoutMS overrides

	mu      sync.Mutex
	entries map[string]*entry // one live heap entry per task id
	h       fireHeap
	running map[string]bool // task id -> a run is in flight
	// deleted records ids Remove dropped, so a spent one-shot's final
	// bookkeeping save can't resurrect the file when Remove lands
	// while that run is still in flight (the id is already out of
	// entries by then). Cleared when the id is registered again.
	deleted map[string]bool

	// wg counts in-flight runTask goroutines so Stop can drain them:
	// once Stop returns, every launched run has finished (or the grace
	// expired) and no fire can land afterwards.
	wg sync.WaitGroup

	wake    chan struct{}
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewScheduler builds a scheduler around runner. A nil runner is
// rejected — every fire needs an executor.
func NewScheduler(runner Runner, defaultTimeout time.Duration) (*Scheduler, error) {
	if runner == nil {
		return nil, fmt.Errorf("timer: NewScheduler requires a runner")
	}
	if defaultTimeout <= 0 {
		defaultTimeout = 10 * time.Minute
	}
	return &Scheduler{
		runner:  runner,
		timeout: defaultTimeout,
		entries: map[string]*entry{},
		running: map[string]bool{},
		deleted: map[string]bool{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}, nil
}

// Start loads every enabled task from $LYON_HOME/timers and runs the
// loop until ctx is cancelled or Stop is called. Missed ticks follow
// the task's miss policy (skip logs and schedules from now; once fires
// one catch-up). Start returns once the load is done, so an Add right
// after Start cannot race the initial registration.
func (s *Scheduler) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		cancel()
		return
	}
	s.started = true
	s.cancel = cancel
	s.done = make(chan struct{}) // fresh, so Stop/Start cycles work
	tasks, err := LoadAll()
	if err != nil {
		LogError("scheduler: load tasks: %v", err)
	}
	now := time.Now()
	loaded := 0
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		if s.registerLocked(t, now) {
			loaded++
		}
	}
	s.mu.Unlock()
	LogInfo("scheduler: started with %d enabled task(s)", loaded)
	go s.loop(ctx)
}

// registerLocked computes t's next fire from now (applying the miss
// policy) and inserts it into the heap. Caller holds s.mu. Returns
// false when the task has no future fire.
func (s *Scheduler) registerLocked(t *Task, now time.Time) bool {
	next, err := computeNext(t, now)
	if err != nil {
		if err != ErrNoNext {
			LogError("timer %s: cannot schedule: %v", t.ID, err)
		} else {
			LogInfo("timer %s: no future fire (spent one-shot)", t.ID)
		}
		return false
	}
	s.insertLocked(t, next)
	return true
}

// insertLocked places t at next in the heap, replacing any existing
// entry for the same id. An entry currently popped by the loop (idx
// -1) is revived and pushed back. Caller holds s.mu.
func (s *Scheduler) insertLocked(t *Task, next time.Time) {
	delete(s.deleted, t.ID) // re-registered ids are no longer delete-guarded
	if e, ok := s.entries[t.ID]; ok {
		e.task = t
		e.next = next
		e.dead = false
		if e.idx >= 0 {
			heap.Fix(&s.h, e.idx)
		} else {
			heap.Push(&s.h, e)
		}
		return
	}
	e := &entry{id: t.ID, task: t, next: next}
	heap.Push(&s.h, e)
	s.entries[t.ID] = e
}

// computeNext resolves a task's first fire at or after now, honoring
// the miss policy: with miss=once, if the tick after LastRun is
// already in the past (at least one fire was lost while lyon was
// down), the task fires one catch-up immediately — never one per
// missed tick.
func computeNext(t *Task, now time.Time) (time.Time, error) {
	next, err := t.Next(now)
	if err != nil {
		return time.Time{}, err
	}
	if t.MissPolicy() == MissOnce && t.LastRun != nil {
		if afterLast, err := t.Next(*t.LastRun); err == nil && afterLast.Before(now) {
			LogInfo("timer %s: catch-up fire (missed at least one tick while down)", t.ID)
			return now, nil
		}
	}
	return next, nil
}

// loop is the single scheduling goroutine: wait for the earliest
// entry, pop everything due, fire, repeat.
func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)
	for {
		s.mu.Lock()
		empty := len(s.h) == 0
		var wait time.Duration
		if !empty {
			wait = time.Until(s.h[0].next)
			if wait < 0 {
				wait = 0
			}
		}
		s.mu.Unlock()

		if empty {
			select {
			case <-ctx.Done():
				s.finish()
				return
			case <-s.wake:
				continue
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.finish()
			return
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
		}

		now := time.Now()
		s.mu.Lock()
		var due []*entry
		for len(s.h) > 0 && !s.h[0].next.After(now) {
			e := heap.Pop(&s.h).(*entry) // e.idx is -1 now, still in map
			due = append(due, e)
		}
		s.mu.Unlock()

		for _, e := range due {
			s.fire(ctx, e, now)
		}
	}
}

// fire reschedules e (unless its schedule is spent or the id was
// re-registered meanwhile), applies the overlap policy, and launches
// the runner goroutine.
func (s *Scheduler) fire(ctx context.Context, e *entry, now time.Time) {
	t := e.task
	s.mu.Lock()
	if e.dead {
		// Removed or disabled while popped — drop the tick entirely.
		if cur, ok := s.entries[t.ID]; ok && cur == e {
			delete(s.entries, t.ID)
		}
		s.mu.Unlock()
		return
	}
	if next, err := t.Next(now); err == nil {
		s.insertLocked(t, next)
	} else {
		delete(s.entries, t.ID)
		// The id leaves the heap before its final run finishes, so
		// mark the task spent: the tail still saves the last
		// bookkeeping (with enabled=false, per the once-kind promise).
		t.spent = true
		LogInfo("timer %s: schedule spent, removed", t.ID)
	}

	if s.running[t.ID] {
		if t.OverlapPolicy() == OverlapQueue {
			// Coalesce: at most one pending catch-up per task; the
			// finishing run services it. Ticks never queue unboundedly.
			if !t.pendingCatchup {
				t.pendingCatchup = true
				LogInfo("timer %s: previous run still going, queued one catch-up", t.ID)
			}
		} else {
			LogWarn("timer %s: previous run still going, skipped this tick", t.ID)
		}
		s.mu.Unlock()
		return
	}
	s.running[t.ID] = true
	// Paired with runTask's deferred Done. Add happens in the loop
	// goroutine, and Stop waits for the loop to exit before draining
	// the WaitGroup, so the counter can never hit zero between an Add
	// and its run finishing.
	s.wg.Add(1)
	s.mu.Unlock()

	go s.runTask(ctx, t)
}

// runTask executes the runner with a timeout, records the outcome on
// the task and in the log, persists the bookkeeping, then services at
// most one coalesced catch-up under the queue policy.
func (s *Scheduler) runTask(ctx context.Context, t *Task) {
	defer s.wg.Done()

	// The scheduler may have stopped between fire() launching this
	// goroutine and the runtime scheduling it — don't run the task at
	// all in that case (and hand the running slot back).
	select {
	case <-ctx.Done():
		LogInfo("timer %s: fire skipped (scheduler stopping)", t.ID)
		s.mu.Lock()
		s.running[t.ID] = false
		s.mu.Unlock()
		return
	default:
	}

	timeout := s.timeout
	if t.TimeoutMS > 0 {
		timeout = time.Duration(t.TimeoutMS) * time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	start := time.Now()
	LogInfo("timer %s: fire (%s)", t.ID, t.Kind)

	err := s.runner(rctx, t)
	cancel()
	dur := time.Since(start).Round(time.Millisecond)

	// A run cut short by Stop (parent cancellation, not the per-run
	// timeout) is an abort, not a task outcome: log it, leave the
	// bookkeeping untouched so a stop can't overwrite the last real
	// status with "context canceled", and drop any armed catch-up —
	// the scheduler is going away. A runner that returned success
	// before the cancellation landed still gets its normal save.
	if ctx.Err() != nil && err != nil {
		LogInfo("timer %s: run aborted (scheduler stopping) after %s", t.ID, dur)
		s.mu.Lock()
		s.running[t.ID] = false
		s.mu.Unlock()
		return
	}

	status := "ok"
	if err != nil {
		status = "error"
		if rctx.Err() == context.DeadlineExceeded {
			status = "timeout"
			err = fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		LogError("timer %s: %s after %s: %v", t.ID, status, dur, err)
	} else {
		LogInfo("timer %s: ok in %s", t.ID, dur)
	}

	now := time.Now()
	s.mu.Lock()
	t.RunCount++
	t.LastRun = &now
	t.LastStatus = status
	if err != nil {
		t.LastError = err.Error()
	} else {
		t.LastError = ""
	}
	// Bookkeeping save and the catch-up decision share one critical
	// section with Remove: a concurrent Remove can't interleave and
	// get its delete undone, and arming a catch-up keeps running=true
	// so no tick can start a second concurrent run in the gap. The
	// write is a tiny JSON file; holding the lock across it is
	// cheaper than a resurrected task file.
	catchup := t.pendingCatchup && t.OverlapPolicy() == OverlapQueue
	t.pendingCatchup = false
	_, registered := s.entries[t.ID]
	// A spent one-shot still gets its final save — with enabled=false,
	// the once-kind promise — even though its id already left the heap.
	if t.spent {
		t.Enabled = false
	}
	if (registered || t.spent) && !s.deleted[t.ID] {
		if serr := Save(t); serr != nil {
			LogError("timer %s: save after run: %v", t.ID, serr)
		}
	}
	if catchup && registered {
		// Keep running[t.ID] set: the catch-up owns the slot. The
		// parent is still WaitGroup-counted here, so this Add is
		// race-free with Stop's drain.
		s.wg.Add(1)
		s.mu.Unlock()
		LogInfo("timer %s: catch-up fire (coalesced ticks)", t.ID)
		go s.runTask(ctx, t)
		return
	}
	s.running[t.ID] = false
	s.mu.Unlock()
}

// Add persists a task and, when enabled, schedules it from now. Safe
// before or after Start; after Start the loop picks it up on its next
// wake.
func (s *Scheduler) Add(t *Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := Save(t); err != nil {
		return err
	}
	if !t.Enabled {
		return nil // persisted only; Enable brings it to life
	}
	s.mu.Lock()
	started := s.started
	ok := s.registerLocked(t, time.Now())
	s.mu.Unlock()
	if started {
		s.kick()
	}
	if !ok {
		return ErrNoNext
	}
	return nil
}

// Remove drops a task from the heap and deletes its file. An in-flight
// run of the id finishes, but its bookkeeping save is suppressed (the
// id is no longer registered, and the deleted ledger blocks a spent
// one-shot's final save), so the file is not resurrected.
func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.dead = true
		if e.idx >= 0 {
			heap.Remove(&s.h, e.idx)
		}
		delete(s.entries, id)
	}
	s.deleted[id] = true
	s.mu.Unlock()
	s.kick()
	return Delete(id)
}

// Enable toggles a persisted task: off removes it from the heap, on
// reschedules it from now. Like Remove, disable lets an already
// in-flight run finish (at most one, by running-flag serialization);
// it only prevents new fires.
func (s *Scheduler) Enable(id string, on bool) error {
	t, err := Load(id)
	if err != nil {
		return err
	}
	t.Enabled = on
	if err := Save(t); err != nil {
		return err
	}
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.dead = true
		if e.idx >= 0 {
			heap.Remove(&s.h, e.idx)
		}
		delete(s.entries, id)
	}
	if on && s.started {
		s.registerLocked(t, time.Now())
	}
	s.mu.Unlock()
	s.kick()
	return nil
}

// stopDrainGrace bounds how long Stop waits for in-flight runs. A
// well-behaved runner returns as soon as its context is cancelled, so
// the drain is effectively instant; the bound only exists so a runner
// that ignores cancellation cannot hang quit forever.
const stopDrainGrace = 2 * time.Second

// Stop ends the loop, waits for it to exit, then drains in-flight
// runs: their context is cancelled, and once Stop returns no further
// fire can land and their bookkeeping saves have completed (a run
// killed by the stop is logged as aborted and does not touch the
// stored status). The drain is bounded by stopDrainGrace; a runner
// that ignores cancellation is abandoned with a WARN and finishes in
// the background. Stop is idempotent and safe before Start.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel, done, wasStarted := s.cancel, s.done, s.started
	s.started = false
	s.mu.Unlock()
	if !wasStarted || cancel == nil {
		return
	}
	cancel()
	<-done
	// All fire() calls happened before the loop exited, so every
	// runTask WaitGroup Add is already paired off by now; a catch-up
	// Add is always done by a still-counted parent run, which keeps
	// this drain race-free.
	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(stopDrainGrace):
		LogWarn("scheduler: stop grace expired; abandoned run(s) will finish in background")
	}
	LogInfo("scheduler: stopped")
}

func (s *Scheduler) finish() {
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
}

// kick nudges the loop to recompute its timer. Never blocks.
func (s *Scheduler) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
