package eval

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ExamInterval is how many production rounds the on-duty agent works
// between held-out exams (design §1.4: every ten rounds, re-qualify).
const ExamInterval = 10

// examFailsLimit: two consecutive failing exams hand the role back to the
// field — the on-duty seat is cleared and the next production score
// re-elects (design §1.4 anti-drift rule).
const examFailsLimit = 2

// historyCap bounds per-agent scorecard history (recent window matters,
// the file must stay small).
const historyCap = 50

// Board is the per-task-class elimination ledger: who is on duty, how many
// production rounds they have served since the last exam, and every
// candidate's scorecard history.
type Board struct {
	Class     string                 `json:"class"`
	OnDuty    string                 `json:"on_duty,omitempty"`
	Rounds    int                    `json:"rounds"` // served since last exam
	ExamFails int                    `json:"exam_fails,omitempty"`
	Agents    map[string][]Scorecard `json:"agents"`
}

// Leaderboard is the file-backed ledger at $LYON_HOME/leaderboard.json.
// All mutations persist immediately — a crash costs at most the in-flight
// scorecard, never the history.
type Leaderboard struct {
	mu      sync.Mutex
	path    string
	Classes map[string]*Board `json:"classes"`
}

// OpenLeaderboard loads the ledger; a missing or corrupt file yields an
// empty board (records start fresh, never fatal).
func OpenLeaderboard() *Leaderboard {
	path := filepath.Join(homeDir(), "leaderboard.json")
	lb := &Leaderboard{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, lb)
	}
	if lb.Classes == nil {
		lb.Classes = map[string]*Board{}
	}
	return lb
}

func (lb *Leaderboard) boardLocked(class string) *Board {
	b, ok := lb.Classes[class]
	if !ok {
		b = &Board{Class: class, Agents: map[string][]Scorecard{}}
		lb.Classes[class] = b
	}
	if b.Agents == nil {
		b.Agents = map[string][]Scorecard{}
	}
	return b
}

// saveLocked writes the ledger atomically (tmp + rename).
func (lb *Leaderboard) saveLocked() error {
	data, err := json.MarshalIndent(lb, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lb.path), 0o755); err != nil {
		return err
	}
	tmp := lb.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, lb.path)
}

func appendCapped(hist []Scorecard, sc Scorecard) []Scorecard {
	hist = append(hist, sc)
	if len(hist) > historyCap {
		hist = hist[len(hist)-historyCap:]
	}
	return hist
}

// pickLocked elects the on-duty agent (design §1.2: among candidates that
// satisfy the condition lines, the highest score takes the job). Eligible
// = latest production card passed; exam-only candidates qualify on their
// latest exam. Mean total over the recent window breaks the contest;
// exact ties go to the lexicographically smaller id so elections replay
// deterministically.
func pickLocked(b *Board) string {
	best, bestAvg := "", -1.0
	for agent, hist := range b.Agents {
		if len(hist) == 0 {
			continue
		}
		latest := hist[len(hist)-1]
		eligible := latest.Passed
		if p := latestProduction(hist); p != nil {
			eligible = p.Passed
		}
		if !eligible {
			continue
		}
		n := len(hist)
		if n > 10 {
			n = 10
		}
		sum := 0.0
		for _, sc := range hist[len(hist)-n:] {
			sum += sc.Total
		}
		avg := sum / float64(n)
		if avg > bestAvg+1e-9 || (math.Abs(avg-bestAvg) <= 1e-9 && (best == "" || agent < best)) {
			best, bestAvg = agent, avg
		}
	}
	return best
}

func latestProduction(hist []Scorecard) *Scorecard {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Source != "exam" {
			return &hist[i]
		}
	}
	return nil
}

// Record files one production scorecard, re-elects the on-duty agent and
// persists. The scorecard's Agent and Source fields identify the attempt.
func (lb *Leaderboard) Record(class string, sc Scorecard) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if sc.At.IsZero() {
		sc.At = time.Now().UTC()
	}
	sc.Class = class
	b := lb.boardLocked(class)
	b.Agents[sc.Agent] = appendCapped(b.Agents[sc.Agent], sc)
	b.OnDuty = pickLocked(b)
	_ = lb.saveLocked()
}

// SubmitExam files an exam scorecard and applies the exam clock: the
// round counter resets (the exam consumed it), a pass clears the failure
// streak, two consecutive failures by the sitting agent demote them
// (OnDuty cleared → next production score re-elects). Returns demoted.
func (lb *Leaderboard) SubmitExam(class string, sc Scorecard) (demoted bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if sc.At.IsZero() {
		sc.At = time.Now().UTC()
	}
	sc.Class = class
	if sc.Source == "" {
		sc.Source = "exam"
	}
	b := lb.boardLocked(class)
	b.Agents[sc.Agent] = appendCapped(b.Agents[sc.Agent], sc)
	b.Rounds = 0
	if sc.Passed {
		b.ExamFails = 0
	} else {
		b.ExamFails++
		if b.ExamFails >= examFailsLimit && b.OnDuty == sc.Agent {
			b.OnDuty = ""
			b.ExamFails = 0
			demoted = true
		}
	}
	// Seat bookkeeping: a demotion leaves the seat OPEN — challengers earn
	// it with production scores, the expelled agent wins it back the same
	// way (design §1.4: two failures reopen the competition, they do not
	// hand the seat to whoever already had it). A vacant seat otherwise
	// promotes on this evidence; a sitting agent whose production evidence
	// has gone stale is re-elected against.
	switch {
	case demoted:
		// OnDuty already cleared above — leave it open.
	case b.OnDuty == "":
		b.OnDuty = pickLocked(b)
	default:
		if p := latestProduction(b.Agents[b.OnDuty]); p != nil && !p.Passed {
			b.OnDuty = pickLocked(b)
		}
	}
	_ = lb.saveLocked()
	return demoted
}

// CountRound books one production round for the class and reports whether
// the on-duty agent's exam is due (every ExamInterval rounds; a vacant
// seat never triggers exams).
func (lb *Leaderboard) CountRound(class string) (rounds int, due bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	b := lb.boardLocked(class)
	b.Rounds++
	rounds = b.Rounds
	due = b.Rounds%ExamInterval == 0 && b.OnDuty != ""
	_ = lb.saveLocked()
	return rounds, due
}

// Status reports the class ledger for UI display: on-duty agent, rounds
// served since the last exam, consecutive exam failures.
func (lb *Leaderboard) Status(class string) (onDuty string, rounds, examFails int) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	b, ok := lb.Classes[class]
	if !ok {
		return "", 0, 0
	}
	return b.OnDuty, b.Rounds, b.ExamFails
}

// OnDuty returns the current representative for a class ("" = open seat).
func (lb *Leaderboard) OnDuty(class string) string {
	onDuty, _, _ := lb.Status(class)
	return onDuty
}

func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lyon"
	}
	return filepath.Join(home, ".lyon")
}
