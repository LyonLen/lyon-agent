package config

import (
	"reflect"
	"testing"
)

func TestParseEmptyAndWhitespaceOnly(t *testing.T) {
	for _, src := range []string{"", "\n", "  \r\n\t\n", "# just a comment\n", "\r\n"} {
		tree, err := parseTOML(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if len(tree) != 0 {
			t.Fatalf("parse %q: want empty tree, got %v", src, tree)
		}
	}
}

func TestParseCRLFWindowsFile(t *testing.T) {
	// Windows editors write CRLF; the parser must treat it as plain EOL.
	src := "# home config\r\nmodel = \"m\"   # trailing comment\r\n\r\n[mcpServers.one]\r\ncommand = \"c\"\r\n"
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tree["model"] != "m" {
		t.Errorf("model = %v, want m", tree["model"])
	}
	tbl, ok := tree["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", tree)
	}
	if tbl["one"].(map[string]any)["command"] != "c" {
		t.Errorf("server table wrong: %v", tbl)
	}
}

func TestParseStringForms(t *testing.T) {
	src := `
basic = "a\"b\nc\\d"
literal = 'C:\raw\path'
uni = "caf\u00e9"
`
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tree["basic"] != "a\"b\nc\\d" {
		t.Errorf("basic = %q", tree["basic"])
	}
	if tree["literal"] != `C:\raw\path` {
		t.Errorf("literal = %q", tree["literal"])
	}
	if tree["uni"] != "café" {
		t.Errorf("uni = %q", tree["uni"])
	}
}

func TestParseMultilineArrayWithComments(t *testing.T) {
	src := `
list = [
  "echo",   # first
  'ls',
  "git",    # last with trailing comma below
]
empty = []
`
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []any{"echo", "ls", "git"}
	if got := tree["list"]; !reflect.DeepEqual(got, want) {
		t.Errorf("list = %v, want %v", got, want)
	}
	if got := tree["empty"]; len(got.([]any)) != 0 {
		t.Errorf("empty = %v", got)
	}
}

func TestParseNumbersAndBools(t *testing.T) {
	src := `
i = 42
neg = -7
f = 0.7
exp = 1e3
negf = -2.5
yes = true
no = false
`
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]any{
		"i": int64(42), "neg": int64(-7), "f": 0.7, "exp": 1000.0,
		"negf": -2.5, "yes": true, "no": false,
	}
	for k, w := range want {
		if tree[k] != w {
			t.Errorf("%s = %v (%T), want %v (%T)", k, tree[k], tree[k], w, w)
		}
	}
}

func TestParseQuotedKeysAndSections(t *testing.T) {
	src := `
"quoted key" = 1
[mcpServers."my server"]
command = "c"
['odd.section']
k = 2
`
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tree["quoted key"] != int64(1) {
		t.Errorf("quoted key = %v", tree["quoted key"])
	}
	tbl := tree["mcpServers"].(map[string]any)
	if tbl["my server"].(map[string]any)["command"] != "c" {
		t.Errorf("quoted section wrong: %v", tbl)
	}
	odd := tree["odd.section"].(map[string]any)
	if odd["k"] != int64(2) {
		t.Errorf("literal dotted section wrong: %v", odd)
	}
}

func TestParseDeepSectionsShareTables(t *testing.T) {
	src := `
[mcpServers.a]
command = "one"

[mcpServers.a.env]
K = "V"

[mcpServers.b]
url = "http://x"
`
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := tree["mcpServers"].(map[string]any)
	a := servers["a"].(map[string]any)
	if a["command"] != "one" {
		t.Errorf("a.command = %v", a["command"])
	}
	if a["env"].(map[string]any)["K"] != "V" {
		t.Errorf("a.env = %v", a["env"])
	}
}

func TestParseSectionReopen(t *testing.T) {
	// Re-opening a table merges into it (lenient superset of strict TOML),
	// so a hand-written file split across blocks still loads.
	src := `
[mcpServers.a]
command = "c"

[other]
k = 1

[mcpServers.a]
args = ["x"]
`
	tree, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := tree["mcpServers"].(map[string]any)["a"].(map[string]any)
	if a["command"] != "c" || !reflect.DeepEqual(a["args"], []any{"x"}) {
		t.Errorf("reopened table = %v", a)
	}
}

func TestParseErrorLineNumbers(t *testing.T) {
	// The line number must point at the offending line, not line 1.
	_, err := parseTOML("model = \"ok\"\neffort = \n")
	if err == nil {
		t.Fatal("want error")
	}
	if got := err.Error(); !(len(got) > 11 && got[:11] == "toml: line ") {
		t.Fatalf("want line-numbered error, got %v", err)
	}
}
