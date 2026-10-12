package main

import (
	"strings"
	"testing"
)

// TestLangSwitch: LYON_LANG seeds the language at startup, /lang flips it
// at runtime, and the flip rebuilds every baked text — the resident
// legend, the popup's descriptions and the /help body — then back again.
// English must survive as the default (the rest of the suite asserts on
// English strings).
func TestLangSwitch(t *testing.T) {
	t.Setenv("LYON_LANG", "zh")
	if got := initLang(); got != "zh" {
		t.Fatalf("initLang with LYON_LANG=zh = %q, want zh", got)
	}
	t.Setenv("LYON_LANG", " en ") // trim + case-insensitive
	if got := initLang(); got != "en" {
		t.Fatalf("initLang with LYON_LANG=\" en \" = %q, want en", got)
	}
	t.Setenv("LYON_LANG", "")
	if got := initLang(); got != "en" {
		t.Fatalf("initLang with LYON_LANG unset = %q, want en (default)", got)
	}

	orig := uiLang
	t.Cleanup(func() { // other tests in this package assume English
		uiLang = orig
		refreshBakedTexts()
	})

	if !setLang("zh") {
		t.Fatal("setLang(zh) must report a change")
	}
	if uiLang != "zh" {
		t.Fatalf("uiLang = %q after setLang(zh)", uiLang)
	}
	if !strings.Contains(idleStatus, "发送") {
		t.Fatalf("resident legend not rebuilt: %q", idleStatus)
	}
	if !strings.Contains(slashCommands[0].desc, "用量表") {
		t.Fatalf("popup descriptions not rebuilt: %q", slashCommands[0].desc)
	}
	if !strings.Contains(langLine(), "zh") {
		t.Fatalf("lang line must state the current language: %q", langLine())
	}
	if help := strings.Join(helpLines(), "\n"); !strings.Contains(help, "本帮助") {
		t.Fatalf("help body not switched to Chinese:\n%s", help)
	}

	if setLang("zh") {
		t.Fatal("repeating the active language must be a no-op")
	}
	if setLang("klingon") {
		t.Fatal("an unknown language argument must be rejected")
	}

	if !setLang("en") {
		t.Fatal("setLang(en) must report a change")
	}
	if !strings.Contains(idleStatus, "enter send") ||
		strings.Contains(idleStatus, "发送") {
		t.Fatalf("legend not restored to English: %q", idleStatus)
	}
	if !strings.Contains(slashCommands[0].desc, "usage table") {
		t.Fatalf("popup descriptions not restored: %q", slashCommands[0].desc)
	}
	if help := strings.Join(helpLines(), "\n"); !strings.Contains(help, "this help") {
		t.Fatalf("help body not restored to English:\n%s", help)
	}
}
