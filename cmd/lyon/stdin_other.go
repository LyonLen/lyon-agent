//go:build !windows

package main

// stdinBytes is the identity off Windows: POSIX stdin is bytes the terminal
// already encoded as UTF-8 (bubbletea's localereader handles the fallback
// decode for the TUI).
func stdinBytes(b []byte) []byte { return b }
