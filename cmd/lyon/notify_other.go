//go:build !windows

package main

// System notifications are Windows-only for now; other platforms get
// silent no-ops so the call sites compile everywhere.

func notifyDone(title, body string) {}

func notifyDecision(title, body string) {}

func truncNotif(s string, n int) string { return s }
