package main

// Terminal bilingual support: every interface string carries an English
// and a Chinese variant; tr() picks the active one. English is the
// default (tests, docs and the open-source screenshots anchor on it) —
// set LYON_LANG=zh at startup, or run /lang zh|en at runtime; the switch
// rebuilds the pre-baked package texts (legend, popup descriptions) and
// every tr() call re-evaluates on the next redraw.
//
// Scope: interface chrome only — status line, popups, help, panels,
// pickers, approval row, slash outputs. Transcript content (prompts,
// assistant text, tool output) is data and is never translated.

import (
	"os"
	"strings"
)

// uiLang is the active interface language: "en" or "zh".
var uiLang = initLang()

func initLang() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LYON_LANG"))) {
	case "zh", "cn", "zh-cn", "zh_cn", "zh-hans", "chinese":
		return "zh"
	}
	return "en"
}

// tr returns the variant for the active interface language.
func tr(en, zh string) string {
	if uiLang == "zh" {
		return zh
	}
	return en
}

// setLang applies an explicit /lang argument ("zh" or "en"). It reports
// whether the language actually changed — a no-op argument or a repeat of
// the current language returns false so the caller can say so.
func setLang(arg string) bool {
	var want string
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "zh", "cn", "zh-cn", "zh_cn", "zh-hans", "chinese":
		want = "zh"
	case "en", "english":
		want = "en"
	default:
		return false
	}
	if want == uiLang {
		return false
	}
	uiLang = want
	// Package-level texts are baked through tr() at init — rebuild them
	// so the legend and popup descriptions follow immediately.
	refreshBakedTexts()
	return true
}

// refreshBakedTexts recomputes every package var whose value was derived
// from tr() during package initialization.
func refreshBakedTexts() {
	idleStatus = tr(idleLegendEN, idleLegendZH)
	slashCommands = buildSlashCommands()
}

// langLine is the /lang answer line in both directions.
func langLine() string {
	return tr("language: en — /lang zh switches to Chinese",
		"界面语言: zh — /lang en 切回英文")
}
