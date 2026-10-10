//go:build windows

package main

import "testing"

func TestNotifyEnvGate(t *testing.T) {
	t.Setenv("LYON_NOTIFY", "off")
	if notifyEnabled() {
		t.Error("LYON_NOTIFY=off must disable notifications")
	}
	t.Setenv("LYON_NOTIFY", "")
	if !notifyEnabled() {
		t.Error("notifications default to enabled")
	}
}

func TestTruncNotif(t *testing.T) {
	if got := truncNotif("a\nb", 10); got != "a b" {
		t.Errorf("newline not flattened: %q", got)
	}
	if got := truncNotif("hello world", 6); got != "hello…" {
		t.Errorf("truncate: %q", got)
	}
}
