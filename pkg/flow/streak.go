package flow

// The §3.1 auto-freeze clock: consecutive green production runs per
// workflow slug, persisted at $LYON_HOME/flow-streak.json. A workflow
// that survives the procedure three times in a row is no longer an
// experiment — the runner freezes it into the user directory as an
// editable artifact (cmd/lyon wires the threshold).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// streakPath is the JSON file holding map[slug]consecutive-green-runs.
func streakPath() string {
	home := os.Getenv("LYON_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			h = "."
		}
		home = filepath.Join(h, ".lyon")
	}
	return filepath.Join(home, "flow-streak.json")
}

var streakMu sync.Mutex

// BumpStreak records one finished run's verdict for a slug and returns
// the new consecutive-green count (0 after a failure). The write is
// atomic (tmp + rename) like the rest of the evolution state.
func BumpStreak(slug string, ok bool) int {
	streakMu.Lock()
	defer streakMu.Unlock()

	counts := map[string]int{}
	if data, err := os.ReadFile(streakPath()); err == nil {
		_ = json.Unmarshal(data, &counts)
	}
	if ok {
		counts[slug]++
	} else {
		counts[slug] = 0
	}
	if err := os.MkdirAll(filepath.Dir(streakPath()), 0o755); err != nil {
		return counts[slug]
	}
	if data, err := json.MarshalIndent(counts, "", "  "); err == nil {
		tmp := streakPath() + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			_ = os.Rename(tmp, streakPath())
		}
	}
	return counts[slug]
}
