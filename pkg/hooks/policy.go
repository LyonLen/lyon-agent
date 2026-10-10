package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Policy is the local security gate — ~/.lyon/policy.json, evaluated
// before approval so a denied tool never bothers the human:
//
//	{
//	  "allow_tools": ["read", "grep", "glob"],        // if set: allowlist mode
//	  "deny_tools": ["bash"],
//	  "deny_patterns": ["(?i)rm\\s+-rf\\s+/", "curl[^|]*\\|\\s*(ba)?sh"]
//	}
//
// allow_tools (non-empty) turns the whole policy into an allowlist: any
// tool not named is denied — the strongest control for locked-down setups.
// deny_patterns are regexes matched against the raw JSON tool input,
// case-insensitive unless the pattern brings its own flags. A missing or
// malformed file means allow-all.
type Policy struct {
	AllowTools   []string `json:"allow_tools,omitempty"`
	DenyTools    []string `json:"deny_tools,omitempty"`
	DenyPatterns []string `json:"deny_patterns,omitempty"`

	allow map[string]bool
	deny  map[string]bool
	res   []policyRe
}

type policyRe struct {
	raw string
	re  *regexp.Regexp
}

func loadPolicy(home string) *Policy {
	p := &Policy{allow: map[string]bool{}, deny: map[string]bool{}}
	b, err := os.ReadFile(filepath.Join(home, "policy.json"))
	if err != nil {
		return p // no file: allow-all
	}
	if json.Unmarshal(b, p) != nil {
		return &Policy{allow: map[string]bool{}, deny: map[string]bool{}} // malformed: allow-all
	}
	for _, t := range p.AllowTools {
		p.allow[strings.ToLower(t)] = true
	}
	for _, t := range p.DenyTools {
		p.deny[strings.ToLower(t)] = true
	}
	for _, pat := range p.DenyPatterns {
		src := pat
		if !strings.HasPrefix(src, "(?") { // bring your own flags or default to CI
			src = "(?i)" + src
		}
		re, err := regexp.Compile(src)
		if err != nil {
			// A pattern that never compiles must not be silently active —
			// skip it; malformed policy is an operational error, not a veto.
			continue
		}
		p.res = append(p.res, policyRe{raw: pat, re: re})
	}
	return p
}

// Check reports whether a tool invocation is denied and which rule did it.
// rule is a human-readable label suitable for audit and model feedback.
func (p *Policy) Check(name string, input []byte) (rule string, denied bool) {
	lname := strings.ToLower(name)
	if len(p.allow) > 0 && !p.allow[lname] {
		return "tool not in allowlist: " + name, true
	}
	if p.deny[lname] {
		return "tool denied: " + name, true
	}
	if len(p.res) > 0 {
		// Raw JSON already contains every string the model passed — no need
		// to walk the structure. Case-insensitivity comes from the pattern
		// itself; add a cheap (?i) fallback only for all-lowercase inputs.
		in := string(input)
		for _, rc := range p.res {
			if rc.re.MatchString(in) {
				return "input matches deny pattern: " + rc.raw, true
			}
		}
	}
	return "", false
}
