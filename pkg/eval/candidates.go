package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Candidate is one entrant of the class competition (design §1.1): a
// {model, effort} profile that sits the same held-out exam as everyone
// else and wins the on-duty seat on evidence, not on config. Candidates
// live as JSON profiles at $LYON_HOME/agents/<id>.json.
type Candidate struct {
	ID      string    `json:"id"`
	Model   string    `json:"model"`
	Effort  string    `json:"effort,omitempty"`
	Note    string    `json:"note,omitempty"`
	Gen     int       `json:"gen,omitempty"` // generations of failure distillation
	Parent  string    `json:"parent,omitempty"`
	Created time.Time `json:"created,omitempty"`
}

// ErrSelfCandidate rejects attempts to overwrite or delete the built-in
// "self" incumbent — it mirrors the running config, not a file.
var ErrSelfCandidate = errors.New("eval: \"self\" is the running model, not a file-backed candidate")

// selfID is the always-present entrant's id.
const selfID = "self"

var candidateID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func agentsDir() string { return filepath.Join(homeDir(), "agents") }

// LoadCandidates reads every profile in $LYON_HOME/agents, sorted by id.
// A corrupt file is skipped — one broken profile must never hide the
// rest of the field.
func LoadCandidates() []Candidate {
	entries, err := os.ReadDir(agentsDir())
	if err != nil {
		return nil
	}
	var out []Candidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(agentsDir(), e.Name()))
		if err != nil {
			continue
		}
		var c Candidate
		if json.Unmarshal(data, &c) != nil || c.ID == "" || c.Model == "" {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SaveCandidate writes one profile atomically (tmp + rename), validating
// the id so a shell-hostile name can never reach the filesystem.
func SaveCandidate(c Candidate) error {
	if c.ID == selfID {
		return ErrSelfCandidate
	}
	if !candidateID.MatchString(c.ID) {
		return fmt.Errorf("eval: candidate id %q must match %s", c.ID, candidateID)
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("eval: candidate model is required")
	}
	if c.Created.IsZero() {
		c.Created = time.Now().UTC()
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := agentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, c.ID+".json")
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	_ = tmp.Chmod(0o644) // CreateTemp makes 0600; keep the file's original mode
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// DeleteCandidate removes one file-backed profile.
func DeleteCandidate(id string) error {
	if id == selfID {
		return ErrSelfCandidate
	}
	if !candidateID.MatchString(id) {
		return fmt.Errorf("eval: candidate id %q must match %s", id, candidateID)
	}
	err := os.Remove(filepath.Join(agentsDir(), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("eval: no candidate %q", id)
	}
	return err
}

// SelfCandidate is the built-in incumbent: the session's own main model,
// always in the field even before anyone has saved a profile.
func SelfCandidate(model string) Candidate {
	if model == "" {
		model = "unknown"
	}
	return Candidate{ID: selfID, Model: model, Note: "running main model"}
}

// Contestants assembles the competition field: every saved profile plus
// the live "self" incumbent (a file claiming id "self" loses to the live
// config — the config is the truth for this process). Sorted by id, so
// contest reports replay in a stable order.
func Contestants(selfModel string) []Candidate {
	saved := LoadCandidates()
	out := make([]Candidate, 0, len(saved)+1)
	for _, c := range saved {
		if c.ID == selfID {
			continue
		}
		out = append(out, c)
	}
	self := SelfCandidate(selfModel)
	out = append(out, self)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
