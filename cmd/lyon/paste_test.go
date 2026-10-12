package main

import (
	"bytes"
	"errors"
	"image"
	imgcolor "image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/tools"
)

// pastePNG makes clipboard-sized image bytes for a stubbed read.
func pastePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, imgcolor.RGBA{R: 200, G: 30, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// stubClipboard hands the TUI a fixed clipboard for one test.
func stubClipboard(t *testing.T, data []byte, paths []string, err error) {
	t.Helper()
	prev := clipPaste
	clipPaste = func() ([]byte, []string, error) { return data, paths, err }
	t.Cleanup(func() { clipPaste = prev })
}

// runPaste presses alt+v (or /paste) and delivers the async result the
// way bubbletea would: the command runs, then the message comes back in.
func runPaste(t *testing.T, m *tuiModel, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("alt+v produced no command")
	}
	msg := cmd()
	pm, ok := msg.(pasteMsg)
	if !ok {
		t.Fatalf("command returned %T, want pasteMsg", msg)
	}
	m.Update(pm)
}

// TestAltVPastesClipboardImage: alt+v stores the picture in the workspace
// and types the @path token that names it, with the dimensions on the
// status line. The token is what makes the picture reach the model —
// PromptBlocks turns it into an image block on submit.
func TestAltVPastesClipboardImage(t *testing.T) {
	ws := tempDir(t)
	m := newTestModel(t)
	m.workspace = ws
	stubClipboard(t, pastePNG(t, 40, 30), nil, nil)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	if !m.pasting {
		t.Fatal("alt+v did not mark a read in flight")
	}
	if m.status != "pasting image…" {
		t.Fatalf("status while reading = %q", m.status)
	}
	runPaste(t, m, cmd)

	if m.pasting {
		t.Fatal("pasting flag not cleared after the read")
	}
	got := m.ti.Value()
	if !strings.Contains(got, "@.lyon/paste/") || !strings.HasSuffix(strings.TrimSpace(got), ".png") {
		t.Fatalf("input = %q, want an @.lyon/paste/*.png token", got)
	}
	if !strings.Contains(m.status, "40x30") {
		t.Fatalf("status = %q, want the dimensions", m.status)
	}
	// The stored file must be the one the token points at, and it must
	// resolve into an image block — otherwise alt+v is only decoration.
	token := strings.TrimPrefix(strings.Fields(strings.TrimSpace(got))[0], "@")
	blocks := tools.PromptBlocks(ws, "look @"+token)
	if len(blocks) != 3 {
		t.Fatalf("PromptBlocks returned %d blocks, want text + note + image", len(blocks))
	}
	if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(token))); err != nil {
		t.Fatalf("stored paste: %v", err)
	}
}

// TestAltVPasteErrorsStayInStatus: a clipboard with nothing to paste must
// say so and must not leave the input dirty or the guard stuck.
func TestAltVPasteErrorsStayInStatus(t *testing.T) {
	m := newTestModel(t)
	m.workspace = tempDir(t)
	stubClipboard(t, nil, nil, errStubClipboard)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	runPaste(t, m, cmd)

	if m.pasting {
		t.Fatal("failed read left the paste guard engaged")
	}
	if !strings.Contains(m.status, "paste:") {
		t.Fatalf("status = %q, want a paste: error", m.status)
	}
	if m.ti.Value() != "" {
		t.Fatalf("input = %q, want empty", m.ti.Value())
	}
}

// errStubClipboard is what the non-Windows clipper returns (and what a
// locked clipboard looks like on Windows).
var errStubClipboard = errors.New("clipboard holds no image")

// TestPasteCommandGuardsReentry: holding alt+v stacks no second read —
// the first one owns the status line.
func TestPasteCommandGuardsReentry(t *testing.T) {
	m := newTestModel(t)
	m.workspace = tempDir(t)
	stubClipboard(t, pastePNG(t, 8, 8), nil, nil)

	_, first := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	_, second := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	if second != nil {
		t.Fatal("a second alt+v started another read while one was in flight")
	}
	runPaste(t, m, first)
	if m.pasting {
		t.Fatal("guard stuck after the read finished")
	}
}

// TestSlashPasteReadsClipboard: /paste is the keyless twin — same async
// read, same token insertion, for people who never learn the shortcut.
func TestSlashPasteReadsClipboard(t *testing.T) {
	m := newTestModel(t)
	m.workspace = tempDir(t)
	stubClipboard(t, pastePNG(t, 12, 9), nil, nil)

	cmd := m.slash("/paste")
	if cmd == nil {
		t.Fatal("/paste produced no command")
	}
	runPaste(t, m, cmd)
	if !strings.Contains(m.ti.Value(), "@.lyon/paste/") {
		t.Fatalf("input = %q, want the attachment token", m.ti.Value())
	}
}
