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
	data, err := json.MarshalIndent(counts, "", "  ")
	if err != nil {
		return counts[slug]
	}
	tmp, err := os.CreateTemp(filepath.Dir(streakPath()), filepath.Base(streakPath())+"-*.tmp")
	if err != nil {
		return counts[slug]
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return counts[slug]
	}
	_ = tmp.Chmod(0o644) // CreateTemp makes 0600; keep the file's original mode
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return counts[slug]
	}
	if err := os.Rename(name, streakPath()); err != nil {
		os.Remove(name)
	}
	return counts[slug]
}
