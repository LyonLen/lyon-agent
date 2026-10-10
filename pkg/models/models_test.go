package models

import (
	"strings"
	"testing"
)

// TestLookupExact: bare ids resolve to themselves before any wildcard.
func TestLookupExact(t *testing.T) {
	got := Lookup("deepseek-chat")
	if got == nil || got.Family != "DeepSeek V3" {
		t.Fatalf("exact lookup: %+v", got)
	}
	// Case-insensitive exact.
	if got := Lookup("DEEPSEEK-CHAT"); got == nil || got.Family != "DeepSeek V3" {
		t.Fatalf("case-insensitive exact: %+v", got)
	}
}

// TestLookupWildcard: dated suffixed ids match the longest pattern —
// gpt-4o-mini-2024-07-18 must land on the mini card, not plain gpt-4o*.
func TestLookupWildcard(t *testing.T) {
	got := Lookup("gpt-4o-mini-2024-07-18")
	if got == nil || got.Family != "GPT-4o mini" {
		t.Fatalf("longest wildcard should win, got %+v", got)
	}
	if got := Lookup("gpt-4o-2024-08-06"); got == nil || got.Family != "GPT-4o" {
		t.Fatalf("gpt-4o dated: %+v", got)
	}
	// Anthropic versioned ids.
	if got := Lookup("claude-sonnet-4-5-20250929"); got == nil ||
		got.Family != "Claude Sonnet 4.5" {
		t.Fatalf("claude dated: %+v", got)
	}
}

// TestLookupMiss: unknown ids return nil — never guess a window.
func TestLookupMiss(t *testing.T) {
	for _, id := range []string{
		"", "   ", "totally-made-up-model", "gpt-5",
		"mimo-v2.6-flash", // the live gateway model — deliberately absent
	} {
		if got := Lookup(id); got != nil {
			t.Fatalf("Lookup(%q) = %+v, want nil", id, got)
		}
	}
}

// TestAllSanity: registry is non-empty, sorted, every entry carries a
// context window and a provider; wildcard ids end with '*'.
func TestAllSanity(t *testing.T) {
	all := All()
	if len(all) < 15 {
		t.Fatalf("registry too small: %d", len(all))
	}
	for i, e := range all {
		if e.Context <= 0 {
			t.Errorf("%s: no context window", e.ID)
		}
		if e.Provider == "" || e.Family == "" {
			t.Errorf("%s: missing provider/family", e.ID)
		}
		if strings.Contains(e.ID, "*") && !strings.HasSuffix(e.ID, "*") {
			t.Errorf("%s: wildcard not trailing", e.ID)
		}
		if i > 0 && all[i-1].ID >= e.ID {
			t.Errorf("registry not sorted: %s >= %s", all[i-1].ID, e.ID)
		}
	}
	// All() returns a copy — mutating it must not corrupt the registry.
	all[0].Family = "MUTATED"
	if fresh := All(); fresh[0].Family == "MUTATED" {
		t.Fatal("All() leaked the internal registry")
	}
}

// TestFormatting: price/context renderers, including the unknown marker.
func TestFormatting(t *testing.T) {
	priced := Lookup("deepseek-chat")
	if priced == nil {
		t.Fatal("deepseek-chat vanished")
	}
	if p := priced.FormatPrice(); !strings.Contains(p, "$0.27") ||
		!strings.Contains(p, "参考价") {
		t.Fatalf("FormatPrice: %q", p)
	}
	unknown := Lookup("claude-opus-5-5")
	if unknown == nil {
		t.Fatal("claude-opus-5-5 vanished")
	}
	if p := unknown.FormatPrice(); !strings.Contains(p, "未知") {
		t.Fatalf("zero pricing should read as unknown: %q", p)
	}
	if c := unknown.FormatContext(); c != "200000" {
		t.Fatalf("FormatContext: %q", c)
	}
}
