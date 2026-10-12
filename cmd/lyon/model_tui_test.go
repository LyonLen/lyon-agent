package main

import (
	"strings"
	"testing"
)

// TestModelCommand: the knowledge card surface — current model, list,
// lookup of an arbitrary id, and the unknown-id fallback that falls
// back to configured values instead of guessing.
func TestModelCommand(t *testing.T) {
	m := newTestModel(t)

	// The ambient model comes from the environment (often a gateway id
	// outside the registry) — assert the shape, not the content: card
	// header + effective config line, known or not.
	m.slash("/model")
	out := m.cmdOutText()
	if !strings.Contains(out, "model card") ||
		!strings.Contains(out, "effective:") {
		t.Fatalf("current model card:\n%s", out)
	}

	// A registered id always renders the full parameter set.
	m.slash("/model deepseek-reasoner")
	out = m.cmdOutText()
	if !strings.Contains(out, "context") ||
		!strings.Contains(out, "DeepSeek R1") {
		t.Fatalf("known card missing params:\n%s", out)
	}

	m.slash("/model deepseek-chat")
	out = m.cmdOutText()
	if !strings.Contains(out, "DeepSeek V3") ||
		!strings.Contains(out, "$0.27") {
		t.Fatalf("lookup card:\n%s", out)
	}
	// Not the live model → card without the effective line.
	if strings.Contains(out, "effective: model=") {
		t.Fatalf("foreign card should not print effective line:\n%s", out)
	}

	m.slash("/model list")
	out = m.cmdOutText()
	if !strings.Contains(out, "known models") ||
		!strings.Contains(out, "gpt-4o") {
		t.Fatalf("model list:\n%s", out)
	}

	m.slash("/model totally-unknown-9000")
	out = m.cmdOutText()
	if !strings.Contains(out, "未收录") {
		t.Fatalf("unknown id fallback:\n%s", out)
	}
}

// TestGroupDigits: thousands separators for context-window display.
func TestGroupDigits(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		999:     "999",
		1000:    "1,000",
		65536:   "65,536",
		200000:  "200,000",
		1047576: "1,047,576",
	}
	for n, want := range cases {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}
