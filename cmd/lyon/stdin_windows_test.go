//go:build windows

package main

import "testing"

// gbkZhiHuiFu is "只回复 ok" in GBK (CP936). Piped through a Chinese
// Windows console this is exactly what `lyon run` used to read, and the
// audit log shows the resulting string: D6 BB is valid UTF-8 (U+05BB), so
// the pair survived as a Hebrew letter, while BB and the following bytes
// became U+FFFD.
var gbkZhiHuiFu = []byte{0xD6, 0xBB, 0xBB, 0xD8, 0xB8, 0xB4, 0x20, 0x6F, 0x6B}

func TestDecodeCodePageGBK(t *testing.T) {
	if cp := stdinCodePage(); cp != 936 {
		t.Skipf("system code page is %d, not 936 (GBK)", cp)
	}
	got := string(decodeCodePage(gbkZhiHuiFu, 936))
	if want := "只回复 ok"; got != want {
		t.Fatalf("decoded to %q, want %q", got, want)
	}
}

// A correct UTF-8 stream must never be re-decoded, even on a GBK machine:
// the same characters in UTF-8 are already valid and pass through verbatim.
func TestDecodeCodePageKeepsUTF8(t *testing.T) {
	in := []byte("只回复 ok")
	if got := string(decodeCodePage(in, 936)); got != string(in) {
		t.Fatalf("valid UTF-8 changed: %q", got)
	}
	ascii := []byte("pwd && ls\r\n")
	if got := string(decodeCodePage(ascii, 936)); got != string(ascii) {
		t.Fatalf("ASCII changed: %q", got)
	}
	if got := decodeCodePage(nil, 936); len(got) != 0 {
		t.Fatalf("nil input should stay empty, got %v", got)
	}
}

// stdinCodePage must resolve to a real code page on either branch (console
// input CP or ANSI CP) — a zero would mean the decode above is dead code.
func TestStdinCodePageResolves(t *testing.T) {
	if cp := stdinCodePage(); cp <= 0 {
		t.Fatalf("stdinCodePage = %d, want a real code page", cp)
	}
}
