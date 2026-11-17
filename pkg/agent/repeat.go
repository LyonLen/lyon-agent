package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// Layer 1 of the runaway guard (docs/planning/WORKORDER-runaway-guard §1):
// fixed-point sealing. A call is 原地打转 exactly when the same
// (tool, canonical input) returned an identical result twice — that is an
// observable zero-information-gain fact, not a round-counting heuristic.
//
// The seal is evidence-based:
//   - first occurrence: execute, record hash+text, count=1;
//   - second occurrence: MUST really execute (external state may have
//     changed — this is the misfire budget that buys zero false positives);
//     identical result → sealed (count=2), changed → progress, reset count;
//   - sealed + read-only (ParallelSafe): short-circuit — the adapter
//     returns the cached text without executing, tail-marked;
//   - sealed + mutating: always execute (side effects may be needed),
//     then append the marker only when the result still matches.
//
// Layer 2 (count wrap-up at 8 / trip at 15) and LYON_REPEAT_GUARD belong
// to commit 3 of §4 — their verdicts are defined here so the verdict set
// matches the workorder, but nothing returns them yet.

// repeatMarker tails a sealed call's result — verbatim from §1.2. The
// leading newline separates it from the (trimmed) result text; after()
// strips exactly this suffix before hashing, so the marker never poisons
// the evidence it annotates.
const repeatMarker = "\n[unchanged · 与第 2 次调用结果完全相同 · 本 run 内该调用已封盘：参数与结果都没变，再调不会得到新信息。要重试请先改变状态或换参数]"

// repeatVerdict is preTool's guard verdict (§1.5.2).
type repeatVerdict int

const (
	// repeatNone: unknown key or not sealed — execute normally.
	repeatNone repeatVerdict = iota
	// repeatShortCircuit: sealed + read-only — skip execution, return the
	// cached result (carried alongside the verdict).
	repeatShortCircuit
	// repeatSealed: sealed + mutating — execute for real, suffix the
	// result when it still matches the sealed hash.
	repeatSealed
	// repeatWrapUp / repeatTrip: Layer 2 (commit 3) — defined so the
	// verdict set is complete; the count logic does not produce them yet.
	repeatWrapUp
	repeatTrip
)

// repeatRecord is one key's run-scoped state (§1.2). text is the sealed
// plain result — needed to hand a short-circuit its cached copy verbatim.
type repeatRecord struct {
	resultHash string
	text       string
	count      int // consecutive identical results
	closed     bool
}

// repeatGuard holds the run-scoped fixed-point state: one map, reset at
// every prompt entry. The zero value is ready to use and every method is
// nil-safe, so a bare &Session{} in tests never panics. preTool runs on
// every goroutine that executes a tool (ParallelSafe calls run
// concurrently), hence the mutex.
type repeatGuard struct {
	mu   sync.Mutex
	last map[string]*repeatRecord

	// blockStreak / trip are Layer 2 state (commit 3): the consecutive
	// no-new-key streak and the trip reason. Carried per §1.5.2.
	blockStreak int
	trip        string
}

func newRepeatGuard() *repeatGuard {
	return &repeatGuard{last: make(map[string]*repeatRecord)}
}

// reset clears run state — called at every prompt entry (§1.5.3): each
// prompt starts with a clean slate, a sealed call from last prompt is
// unknown again.
func (g *repeatGuard) reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.last = make(map[string]*repeatRecord)
	g.blockStreak = 0
	g.trip = ""
	g.mu.Unlock()
}

// before is preTool's guard check, run AFTER the security gates (a call
// the policy rejected is not guard evidence). parallel is the tool's
// ParallelSafe — static per tool — and only read-only calls may
// short-circuit: a mutating tool's side effects are never skipped.
// Returns the verdict and, for repeatShortCircuit, the cached result
// text to hand back verbatim.
func (g *repeatGuard) before(name string, input json.RawMessage, parallel bool) (repeatVerdict, string) {
	if g == nil {
		return repeatNone, ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.last[repeatKey(name, input)]
	if rec == nil || !rec.closed {
		return repeatNone, ""
	}
	if parallel {
		return repeatShortCircuit, rec.text
	}
	return repeatSealed, ""
}

// after feeds one executed result (ResultObserver seam). The result may
// carry the repeatMarker suffix — it is stripped before hashing so the
// marker never counts as a changed result. Returns true when this call
// ESTABLISHED the seal (identical result ×2) — the audit's evidence.
func (g *repeatGuard) after(name string, input json.RawMessage, result string) bool {
	if g == nil {
		return false
	}
	plain := strings.TrimSpace(strings.TrimSuffix(result, repeatMarker))
	if plain == "" {
		return false // empty results are not fixed-point evidence
	}
	h := resultHash(plain)

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = make(map[string]*repeatRecord)
	}
	key := repeatKey(name, input)
	rec := g.last[key]
	switch {
	case rec == nil:
		g.last[key] = &repeatRecord{resultHash: h, text: plain, count: 1}
	case rec.resultHash != h:
		// Progress: a different result restarts the chain (§1.2 row 2).
		rec.resultHash, rec.text, rec.count, rec.closed = h, plain, 1, false
	default:
		rec.count++
		if rec.count >= 2 && !rec.closed {
			rec.closed = true
			return true
		}
	}
	return false
}

// suffixFor returns the marker-appender for one key. The adapter calls it
// after capResults, before the observer defer — at which point a sealed
// key's recorded hash is either kept (identical → append) or about to be
// reset by after() (changed → the check already says no), so evaluating
// against pre-after() state is exact for this execution.
func (g *repeatGuard) suffixFor(name string, input json.RawMessage) func(string) string {
	key := repeatKey(name, input)
	return func(plain string) string {
		if g == nil {
			return ""
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		rec := g.last[key]
		if rec == nil || !rec.closed || rec.resultHash != resultHash(plain) {
			return ""
		}
		return repeatMarker
	}
}

// repeatInstruction is the structured PreTool result for a sealed key —
// the sentinel the adapter recognizes with errors.As (§1.5.3's seam).
// Error() exists only to satisfy `error`; it is never surfaced to the
// model: Execute converts the instruction into a success result.
type repeatInstruction struct {
	cached    string
	haveCache bool
	suffix    func(string) string
}

func (r *repeatInstruction) Error() string { return "repeat guard: sealed key" }

func (r *repeatInstruction) CachedResult() (string, bool) { return r.cached, r.haveCache }

func (r *repeatInstruction) ResultSuffix() func(string) string { return r.suffix }

// repeatKey namespaces a key by tool: two tools sharing byte-identical
// input must never collide.
func repeatKey(name string, input json.RawMessage) string {
	return name + "\x00" + canonicalInput(input)
}

// canonicalInput applies the §1.6 recipe — marshal → decode (UseNumber)
// into any → normalize numbers → re-marshal (map keys sorted) — then
// sha256[:16]. Key order and whitespace stop mattering; 1, 1.0 and 1.00
// hash alike (the equivalence test). Non-JSON input hashes verbatim.
func canonicalInput(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return hash16(string(raw))
	}
	if _, err := dec.Token(); err != io.EOF {
		return hash16(string(raw)) // trailing junk: not one JSON value
	}
	out, err := json.Marshal(normalizeNumbers(v))
	if err != nil {
		return hash16(string(raw))
	}
	return hash16(string(out))
}

// resultHash fingerprints one result (§1.6): a result that IS a single
// JSON value canonicalizes like an input (object keys sorted, 1 == 1.0);
// any ordinary text hashes verbatim.
func resultHash(s string) string {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return hash16(s)
	}
	if _, err := dec.Token(); err != io.EOF {
		return hash16(s)
	}
	out, err := json.Marshal(normalizeNumbers(v))
	if err != nil {
		return hash16(s)
	}
	return hash16(string(out))
}

// normalizeNumbers folds json.Number into float64 so 1 and 1.0 (and
// 1.00) canonicalize to the same bytes; values float64 cannot parse
// keep their literal text. map/slice values are rewritten in place —
// the tree was decoded fresh by the caller.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeNumbers(e)
		}
		return t
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	default:
		return v
	}
}

// hash16 is sha256 trimmed to 16 hex chars — plenty for run-scoped keys.
func hash16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
