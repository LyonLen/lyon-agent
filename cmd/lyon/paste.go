package main

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/tools"
)

// alt+v — paste a picture from the clipboard, the way a chat app does.
// The clipboard can't reach the model directly (it only speaks @path), so
// the pixels are stored in the workspace and the input gets the token that
// resolves to them; sending works exactly like a typed @path.

// pasteMsg is the outcome of one clipboard read: the stored attachment
// and its dimensions, or why nothing could be stored.
type pasteMsg struct {
	rel  string
	w, h int
	err  error
}

// clipPaste is the platform clipboard reader (clip_windows.go, and the
// honest error in clip_other.go). A variable so tests can hand the TUI a
// picture without touching a real clipboard.
var clipPaste = clipPastePlatform

// pasteCmd reads the clipboard off the UI goroutine: the read may wait on
// another process's clipboard lock for half a second, and the TUI must
// keep painting while it does.
func pasteCmd(root string) tea.Cmd {
	return func() tea.Msg { return readPaste(root) }
}

// readPaste is the whole paste in one blocking call — the TUI wraps it in
// pasteCmd, the plain REPL (main.go) just prints the result. Nothing in
// here needs a message loop.
func readPaste(root string) pasteMsg {
	data, paths, err := clipPaste()
	switch {
	case err != nil:
		return pasteMsg{err: err}
	case len(data) > 0:
		rel, _, w, h, err := tools.PasteImage(root, data)
		return pasteMsg{rel: rel, w: w, h: h, err: err}
	case len(paths) > 0:
		rel, _, w, h, err := tools.PasteFile(root, paths[0])
		return pasteMsg{rel: rel, w: w, h: h, err: err}
	default:
		return pasteMsg{err: errors.New("clipboard holds no image — copy a screenshot or an image file first")}
	}
}

// attachPaste folds a finished paste into the input. The token is typed
// like any other keystroke: the cursor stays wherever the user left it,
// and history and the suggestion popup see exactly what they would have
// seen had it been typed by hand.
func (m *tuiModel) attachPaste(p pasteMsg) {
	m.pasting = false
	switch {
	case p.err != nil:
		m.status = tr("paste: ", "粘贴失败: ") + sanitizeText(p.err.Error())
	case p.rel == "":
		m.status = tr("paste: nothing to attach", "粘贴: 没有可附的内容")
	default:
		m.ti, _ = m.ti.Update(tea.KeyMsg{Type: tea.KeyRunes,
			Runes: []rune("@" + p.rel + " ")})
		m.updateSuggestions()
		if p.w > 0 && p.h > 0 {
			m.status = tr(fmt.Sprintf("attached @%s (%dx%d)", p.rel, p.w, p.h),
				fmt.Sprintf("已附上 @%s（%dx%d）", p.rel, p.w, p.h))
		} else {
			m.status = tr("attached @", "已附上 @") + p.rel
		}
	}
}
