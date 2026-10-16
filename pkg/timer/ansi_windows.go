//go:build windows

package timer

import (
	"syscall"
	"unsafe"
)

// Localized Windows tools print their messages in the system's ANSI
// code page when stdout is a pipe: on Chinese Windows that is GBK
// (ACP 936). schtasks is no exception — its "找不到" / "拒绝访问"
// messages arrive as GBK bytes, and raw UTF-8 byte matching never
// hits. ansiToUTF8 decodes b from CP_ACP to UTF-8 so every match and
// error message downstream works on text; pure ASCII passes through
// unchanged.
func ansiToUTF8(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	n, _, _ := procMultiByteToWideChar.Call(
		0, /* CP_ACP */
		0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		0, 0)
	if n == 0 {
		return b
	}
	u16 := make([]uint16, n)
	n, _, _ = procMultiByteToWideChar.Call(
		0, 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&u16[0])), n)
	if n == 0 {
		return b
	}
	utf8Len, _, _ := procWideCharToMultiByte.Call(
		65001, /* CP_UTF8 */
		0,
		uintptr(unsafe.Pointer(&u16[0])), n,
		0, 0, 0, 0)
	if utf8Len == 0 {
		return b
	}
	out := make([]byte, utf8Len)
	procWideCharToMultiByte.Call(
		65001, 0,
		uintptr(unsafe.Pointer(&u16[0])), n,
		uintptr(unsafe.Pointer(&out[0])), utf8Len, 0, 0)
	return out
}

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procMultiByteToWideChar = kernel32.NewProc("MultiByteToWideChar")
	procWideCharToMultiByte = kernel32.NewProc("WideCharToMultiByte")
	procGetACP              = kernel32.NewProc("GetACP")
)

// ansiCodePage exposes GetACP for tests (CP936-specific fixtures are
// skipped on machines with another ANSI code page).
func ansiCodePage() int {
	r, _, _ := procGetACP.Call()
	return int(r)
}
