package hooks

import (
	"encoding/json"
	"os"
	"time"
)

// Stats are the aggregates behind /stats and lyond metrics: how often each
// tool ran, how long, how often it failed, token spend, approval and
// security outcomes. Persisted to $LYON_HOME/stats.json on prompt_end and
// explicit Save, so a crash costs at most one prompt of counters.
type Stats struct {
	Prompts        int64 `json:"prompts"`
	ApiCalls       int64 `json:"api_calls"`
	Compactions    int64 `json:"compactions"`
	Subagents      int64 `json:"subagents"`
	ApprovalsReq   int64 `json:"approvals_requested"`
	ApprovalsYes   int64 `json:"approvals_yes"`
	ApprovalsNo    int64 `json:"approvals_no"`
	SecurityBlocks int64 `json:"security_blocks"`
	HookFailures   int64 `json:"hook_failures"`
	AuditDropped   int64 `json:"audit_dropped,omitempty"`

	Tokens Tokens `json:"tokens"`

	Tools map[string]*ToolStats `json:"tools,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	LastActive time.Time `json:"last_active"`
}

// Tokens sums token usage across turns and subagents for this stats file.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// ToolStats is one tool's execution record.
type ToolStats struct {
	Calls   int64 `json:"calls"`
	Errors  int64 `json:"errors"`
	TotalMS int64 `json:"total_ms"`
}

// copy returns a snapshot safe to hand out without holding the lock.
func (s Stats) copy() Stats {
	out := s
	if s.Tools != nil {
		out.Tools = make(map[string]*ToolStats, len(s.Tools))
		for k, v := range s.Tools {
			cp := *v
			out.Tools[k] = &cp
		}
	}
	return out
}

// loadStats reads stats.json; a missing or corrupt file starts fresh but
// keeps a stable StartedAt for "this install" age display.
func loadStats(path string) Stats {
	s := Stats{StartedAt: time.Now()}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if json.Unmarshal(b, &s) != nil {
		return Stats{StartedAt: time.Now()}
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now()
	}
	return s
}
