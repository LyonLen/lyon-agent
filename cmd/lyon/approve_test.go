package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/tools"
)

// TestChatApproveOn: explicit flags always win; otherwise a TTY asks
// (default mode now means "ask a human") and a piped stdin stays silent
// so scripts can never block on y/n.
func TestChatApproveOn(t *testing.T) {
	on, off := true, false
	cases := []struct {
		set  bool
		v    *bool
		tty  bool
		want bool
	}{
		{false, &on, true, true},   // interactive default: ask
		{false, &on, false, false}, // piped stdin: never block
		{true, &on, false, true},   // explicit -approve on a pipe
		{true, &off, true, false},  // explicit -approve=false
	}
	for i, c := range cases {
		if got := chatApproveOn(c.set, c.v, c.tty); got != c.want {
			t.Errorf("case %d: chatApproveOn(set=%v, *v=%v, tty=%v) = %v, want %v",
				i, c.set, *c.v, c.tty, got, c.want)
		}
	}
}

// TestApproveAlways: "a" approves once and vouches for the tool for the
// rest of the session — later requests short-circuit with no overlay;
// other tools still ask; "n" never persists.
func TestApproveAlways(t *testing.T) {
	m := newTestModel(t)
	send := func(name, input string) chan bool {
		ch := make(chan bool, 1)
		m.Update(approveReq{ev: tools.ExecEvent{Name: name, Input: []byte(input)}, ch: ch})
		return ch
	}

	ch1 := send("run_shell", `{"command":"ls"}`)
	if m.appr == nil {
		t.Fatal("first request must open the overlay")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.appr != nil {
		t.Fatal("a must close the overlay")
	}
	if !<-ch1 {
		t.Fatal("a must answer approve")
	}

	// Same tool again: pre-authorized, no overlay, granted directly.
	ch2 := send("run_shell", `{"command":"rm x"}`)
	if m.appr != nil {
		t.Fatal("pre-approved tool must not re-open the overlay")
	}
	if !<-ch2 {
		t.Fatal("pre-authorized request must be granted")
	}

	// A different tool still asks, and n denies without persisting.
	ch3 := send("write_file", `{"path":"a.txt"}`)
	if m.appr == nil {
		t.Fatal("write_file must still ask")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.appr != nil {
		t.Fatal("n must close the overlay")
	}
	if <-ch3 {
		t.Fatal("n must answer deny")
	}
	checkView(t, m, "approval overlay frame")
}
