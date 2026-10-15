// This file is the hand-rolled TOML subset parser lyon.toml needs. The
// supported grammar is documented in the package comment of config.go; the
// short version is: comments, bare/quoted keys, [section] headers, basic
// and literal strings, booleans, integers, floats, and arrays of scalars —
// no inline tables, no dotted-key assignments, no dates. Every error path
// returns (never panics) and carries a line number so a broken config.toml
// is diagnosable from one stderr line.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// tomlParser walks src one byte at a time. Bounds are checked before every
// index; p.pos may momentarily pass len(src) after a truncated escape, but
// every loop re-checks the bound first.
type tomlParser struct {
	src  string
	pos  int
	line int
}

// parseTOML parses src into a tree: map[string]any for tables, plus string,
// bool, int64, float64 and []any for values. Duplicate keys and section/
// value collisions are errors.
func parseTOML(src string) (map[string]any, error) {
	p := &tomlParser{src: src, line: 1}
	root := map[string]any{}
	cur := root
	for {
		p.skipBlanks()
		if p.pos >= len(p.src) {
			return root, nil
		}
		if p.src[p.pos] == '[' {
			path, err := p.sectionHeader()
			if err != nil {
				return nil, err
			}
			tbl, err := sectionTable(root, path)
			if err != nil {
				return nil, p.errorf("%s", err)
			}
			cur = tbl
			if err := p.endOfLine(); err != nil {
				return nil, err
			}
			continue
		}
		key, err := p.keyPart()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '.' {
			return nil, p.errorf("dotted keys are not supported — use a [section] header")
		}
		if p.pos >= len(p.src) || p.src[p.pos] != '=' {
			return nil, p.errorf("want = after key %q", key)
		}
		p.pos++
		p.skipSpace()
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		if _, dup := cur[key]; dup {
			return nil, p.errorf("duplicate key %q", key)
		}
		cur[key] = val
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

// sectionTable navigates (creating as needed) the table a [a.b.c] header
// names. A path segment that already holds a non-table value is an error.
func sectionTable(root map[string]any, path []string) (map[string]any, error) {
	tbl := root
	for i, part := range path {
		v, ok := tbl[part]
		if !ok {
			nt := map[string]any{}
			tbl[part] = nt
			tbl = nt
			continue
		}
		nt, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("section [%s]: %q is already a value, not a table",
				strings.Join(path[:i+1], "."), part)
		}
		tbl = nt
	}
	return tbl, nil
}

// sectionHeader consumes "[a.b.c]" and returns the path.
func (p *tomlParser) sectionHeader() ([]string, error) {
	p.pos++ // '['
	p.skipSpace()
	var path []string
	for {
		part, err := p.keyPart()
		if err != nil {
			return nil, err
		}
		path = append(path, part)
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '.' {
			p.pos++
			p.skipSpace()
			continue
		}
		break
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ']' {
		return nil, p.errorf("want ] to close the section header")
	}
	p.pos++
	return path, nil
}

// keyPart consumes one key segment: bare [A-Za-z0-9_-]+ or a quoted string.
func (p *tomlParser) keyPart() (string, error) {
	if p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '"':
			return p.basicString()
		case '\'':
			return p.literalString()
		}
	}
	start := p.pos
	for p.pos < len(p.src) && isBareKeyChar(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return "", p.errorf("want a key")
	}
	return p.src[start:p.pos], nil
}

func isBareKeyChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' || c == '_' || c == '-'
}

// value consumes one scalar or array value.
func (p *tomlParser) value() (any, error) {
	if p.pos >= len(p.src) {
		return nil, p.errorf("want a value")
	}
	switch c := p.src[p.pos]; {
	case c == '"':
		return p.basicString()
	case c == '\'':
		return p.literalString()
	case c == '[':
		return p.array()
	case c == 't':
		if strings.HasPrefix(p.src[p.pos:], "true") {
			p.pos += 4
			return true, nil
		}
		return nil, p.errorf("invalid value")
	case c == 'f':
		if strings.HasPrefix(p.src[p.pos:], "false") {
			p.pos += 5
			return false, nil
		}
		return nil, p.errorf("invalid value")
	case c == '+' || c == '-' || c >= '0' && c <= '9':
		return p.number()
	default:
		return nil, p.errorf("invalid value")
	}
}

// number consumes an integer or float. No underscores, hex or octal — a
// "0x" prefix simply ends the number at '0' and the stray 'x' surfaces as
// an "unexpected character" on the same line.
func (p *tomlParser) number() (any, error) {
	start := p.pos
	p.pos++ // sign or first digit
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E':
			p.pos++
		case (c == '+' || c == '-') && p.pos > start &&
			(p.src[p.pos-1] == 'e' || p.src[p.pos-1] == 'E'):
			p.pos++
		default:
			goto done
		}
	}
done:
	s := p.src[start:p.pos]
	if !strings.ContainsAny(s, ".eE") {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, p.errorf("invalid integer %q", s)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, p.errorf("invalid float %q", s)
	}
	return f, nil
}

// basicString consumes a "double-quoted" string. Escapes are validated by
// strconv.Unquote, which accepts a superset of TOML's escape set (a stray
// \a slips through — harmless for a config surface).
func (p *tomlParser) basicString() (string, error) {
	start := p.pos
	p.pos++ // opening quote
	for p.pos < len(p.src) && p.src[p.pos] != '"' {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
		case '\n':
			return "", p.errorf("newline in basic string")
		default:
			p.pos++
		}
	}
	if p.pos >= len(p.src) {
		return "", p.errorf("unterminated string")
	}
	raw := p.src[start : p.pos+1]
	p.pos++
	s, err := strconv.Unquote(raw)
	if err != nil {
		return "", p.errorf("bad string escape: %v", err)
	}
	return s, nil
}

// literalString consumes a 'single-quoted' string (no escapes).
func (p *tomlParser) literalString() (string, error) {
	start := p.pos
	p.pos++ // opening quote
	for p.pos < len(p.src) && p.src[p.pos] != '\'' {
		if p.src[p.pos] == '\n' {
			return "", p.errorf("newline in literal string")
		}
		p.pos++
	}
	if p.pos >= len(p.src) {
		return "", p.errorf("unterminated string")
	}
	s := p.src[start+1 : p.pos]
	p.pos++
	return s, nil
}

// array consumes "[v, v, ...]" — multi-line, comments allowed, trailing
// comma accepted. Elements may be any values; the decoder insists on a
// homogeneous string array where the schema requires one.
func (p *tomlParser) array() ([]any, error) {
	p.pos++ // '['
	out := []any{}
	for {
		p.skipBlanks()
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated array")
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipBlanks()
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated array")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, p.errorf("want , or ] in array, got %q", p.src[p.pos])
		}
	}
}

// skipBlanks consumes whitespace, newlines (counting lines) and comments.
func (p *tomlParser) skipBlanks() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\r':
			p.pos++
		case '\n':
			p.pos++
			p.line++
		case '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// skipSpace consumes spaces, tabs and carriage returns — not newlines.
func (p *tomlParser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\r':
			p.pos++
		default:
			return
		}
	}
}

// endOfLine consumes the rest of a logical line: trailing spaces, an
// optional comment, then the newline (CRLF included — Windows editors).
func (p *tomlParser) endOfLine() error {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil
	}
	switch p.src[p.pos] {
	case '#':
		for p.pos < len(p.src) && p.src[p.pos] != '\n' {
			p.pos++
		}
	case '\n':
		p.pos++
		p.line++
	default:
		return p.errorf("unexpected %q after the value on this line", p.src[p.pos])
	}
	return nil
}

func (p *tomlParser) errorf(format string, args ...any) error {
	return fmt.Errorf("toml: line %d: %s", p.line, fmt.Sprintf(format, args...))
}
