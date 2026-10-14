package main

// The @ file popup (#64): typing an @-token in the input browses the
// workspace the way the slash popup browses commands — directories
// first, ".." to climb, ↑/↓ to choose, enter/tab to insert. Paths land
// workspace-relative ("@pkg/rea" → "@pkg/readme.md "), the same form
// tools.PromptBlocks resolves at submit time. Quoted paths with spaces
// stay a manual escape hatch — the popup only claims bare tokens.

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// atToken extracts the trailing @path token being completed: the last
// whitespace-separated field, iff it starts with "@" and holds no space
// or quote after the sigil. Returns the token without "@" and its start
// byte offset in the input.
func atToken(v string) (tok string, start int, ok bool) {
	if v == "" {
		return "", 0, false
	}
	start = strings.LastIndexAny(v, " \t") + 1 // 0 when no space
	tok = v[start:]
	if len(tok) < 1 || tok[0] != '@' {
		return "", 0, false
	}
	rest := tok[1:]
	if strings.ContainsAny(rest, " \t\"") {
		return "", 0, false // quoted paths are typed by hand
	}
	return rest, start, true
}

// atMaxEntries caps the popup so a huge directory can't drown the input
// line; the filter prefix is the intended way to narrow anyway.
const atMaxEntries = 40

// atEntries lists workspace entries for one @ token: dirs first, then
// files, alphabetical within each group; ".." leads whenever we're
// already inside a subdirectory; dotfiles appear only under a "."-typed
// prefix. The token's directory part is confined to the workspace — a
// "../…" escape lists nothing.
func atEntries(workspace, rel string) []sugItem {
	dir, prefix := path.Split(rel) // dir keeps its trailing "/" ("" at root)
	root := workspace
	if dir != "" {
		joined := filepath.Join(workspace, filepath.FromSlash(dir))
		if !withinDir(workspace, joined) {
			return nil
		}
		root = joined
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	prefix = strings.ToLower(prefix)

	var dirs, files []sugItem
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue // hidden until asked for, like the shells
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, sugItem{
				cmd:  "@" + dir + name + "/",
				desc: tr("dir", "目录"),
				cat:  "file",
			})
			continue
		}
		desc := ""
		if info, err := e.Info(); err == nil {
			desc = formatBytes(info.Size())
		}
		files = append(files, sugItem{
			cmd:  "@" + dir + name,
			desc: desc,
			cat:  "file",
		})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].cmd < dirs[j].cmd })
	sort.Slice(files, func(i, j int) bool { return files[i].cmd < files[j].cmd })

	out := make([]sugItem, 0, len(dirs)+len(files)+1)
	if dir != "" {
		// Climb: the parent of "pkg/sub/" is "pkg/"; of "pkg/" it's the
		// root (bare "@" + nothing to append).
		parent := path.Dir(strings.TrimSuffix(dir, "/"))
		insert := ""
		if parent != "." && parent != "/" && parent != "" {
			insert = parent + "/"
		}
		out = append(out, sugItem{cmd: "@" + insert, desc: tr("up one dir", "上一级目录"), cat: "file"})
	}
	for _, d := range dirs {
		if len(out) >= atMaxEntries {
			return out
		}
		out = append(out, d)
	}
	for _, f := range files {
		if len(out) >= atMaxEntries {
			return out
		}
		out = append(out, f)
	}
	return out
}

// atPopup reports whether the file popup owns the current input: the
// input ends in a bare @token and the popup isn't dismissed.
func (m *tuiModel) atPopup() bool {
	if m.sugHide || m.appr != nil {
		return false
	}
	_, _, ok := atToken(m.ti.Value())
	return ok
}

// insertAtSelection replaces the trailing @token with the highlighted
// entry: a directory keeps the trailing "/" and stays open to descend,
// a file gets a trailing space and the popup closes on its own (the
// finished token no longer matches anything).
func (m *tuiModel) insertAtSelection() {
	if m.sugIdx < 0 || m.sugIdx >= len(m.sugs) {
		return
	}
	picked := m.sugs[m.sugIdx].cmd
	v := m.ti.Value()
	_, start, ok := atToken(v)
	if !ok {
		return
	}
	nv := v[:start] + picked
	if !strings.HasSuffix(picked, "/") {
		nv += " "
	}
	m.ti.SetValue(nv)
	m.ti.CursorEnd()
	m.updateSuggestions()
}

// withinDir reports whether child stays inside root (both absolute).
func withinDir(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// formatBytes renders a file size the way a listing row wants it.
func formatBytes(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1024*1024:
		return strconv.FormatInt(n/1024, 10) + " KB"
	default:
		return strconv.FormatInt(n/(1024*1024), 10) + " MB"
	}
}
