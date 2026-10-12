//go:build !windows

package main

import (
	"fmt"
	"runtime"
)

// clipPastePlatform is the non-Windows half of the alt+v paste. Windows
// reads the clipboard through Win32 (clip_windows.go); the other platforms
// do not have an implementation yet, and a paste that silently does
// nothing would be worse than one that says so — the @path attachment
// syntax still covers them (save the file into the workspace and type
// @shot.png).
func clipPastePlatform() (data []byte, paths []string, err error) {
	return nil, nil, fmt.Errorf("clipboard image paste is not supported on %s yet — save the file into the workspace and attach it with @path", runtime.GOOS)
}
