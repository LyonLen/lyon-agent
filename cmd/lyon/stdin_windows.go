//go:build windows

package main

// Console stdin on Windows delivers bytes in a legacy code page (936/GBK on
// a Chinese Windows, as `chcp` reports), not UTF-8. Reading those bytes
// straight into a Go string produced the garbled Chinese prompts in the
// audit log and the session snapshots: the GBK pairs that happened to be
// valid UTF-8 survived as the wrong rune, every other byte became U+FFFD.
// The bubbletea TUI is immune — it reads UTF-16 KEY_EVENT records through
// coninput — but the bare reads were not: `lyon run` from piped stdin, the
// -plain REPL, and the y/n approval prompt.
//
// stdinBytes decodes that input, conservatively: bytes that are already
// valid UTF-8 (a UTF-8 pipe, an English prompt) are returned untouched, so a
// correct stream can never be corrupted in the name of fixing a broken one.

import (
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stdinBytes returns b as UTF-8: unchanged when it already is, otherwise
// re-decoded from the code page stdin is actually delivering (the console
// input CP for a console handle, the ANSI CP for a pipe). A failed decode
// also passes b through — mangled bytes beat a silently empty prompt.
func stdinBytes(b []byte) []byte {
	return decodeCodePage(b, stdinCodePage())
}

// stdinCodePage is the code page raw stdin bytes arrive in: the console
// input code page when stdin is a console, else the system ANSI code page
// that every console-writing program uses for a pipe.
func stdinCodePage() int {
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(windows.Stdin), uintptr(unsafe.Pointer(&mode))); r != 0 {
		if cp, _, _ := procGetConsoleCP.Call(); cp != 0 {
			return int(cp)
		}
	}
	cp, _, _ := procGetACP.Call()
	return int(cp)
}

// decodeCodePage turns b into UTF-8 from an explicit code page. Valid UTF-8
// and UTF-8 consoles pass through; anything else goes through
// MultiByteToWideChar → UTF-16 → WideCharToMultiByte → UTF-8, the same
// translation pkg/timer's ansiToUTF8 applies to schtasks output.
func decodeCodePage(b []byte, cp int) []byte {
	if len(b) == 0 || utf8.Valid(b) || cp <= 0 || cp == 65001 {
		return b
	}
	if out := codePageToUTF8(b, cp); len(out) > 0 {
		return out
	}
	return b
}

func codePageToUTF8(b []byte, cp int) []byte {
	n, _, _ := procMultiByteToWideChar.Call(
		uintptr(cp),
		0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		0, 0)
	if n == 0 {
		return nil
	}
	u16 := make([]uint16, n)
	n, _, _ = procMultiByteToWideChar.Call(
		uintptr(cp), 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&u16[0])), n)
	if n == 0 {
		return nil
	}
	utf8Len, _, _ := procWideCharToMultiByte.Call(
		65001, /* CP_UTF8 */
		0,
		uintptr(unsafe.Pointer(&u16[0])), n,
		0, 0, 0, 0)
	if utf8Len == 0 {
		return nil
	}
	out := make([]byte, utf8Len)
	procWideCharToMultiByte.Call(
		65001, 0,
		uintptr(unsafe.Pointer(&u16[0])), n,
		uintptr(unsafe.Pointer(&out[0])), utf8Len, 0, 0)
	return out
}

// kernel32 is already resolved by clip_windows.go; these are the code page
// entry points, kept next to their only user.
var (
	procMultiByteToWideChar = kernel32.NewProc("MultiByteToWideChar")
	procWideCharToMultiByte = kernel32.NewProc("WideCharToMultiByte")
	procGetACP              = kernel32.NewProc("GetACP")
	procGetConsoleCP        = kernel32.NewProc("GetConsoleCP")
	procGetConsoleMode      = kernel32.NewProc("GetConsoleMode")
)
