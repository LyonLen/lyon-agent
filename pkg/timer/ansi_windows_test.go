//go:build windows

package timer

import "testing"

// GBK (CP936) bytes of "错误: 系统找不到指定的文件。" exactly as
// Chinese Windows prints them through a pipe (starts B4 ED CE F3 =
// 错误, the byte prefix observed in real schtasks output).
var gbkMissingTask = []byte{
	0xB4, 0xED, 0xCE, 0xF3, 0x3A, 0x20, 0xCF, 0xB5, 0xCD, 0xB3,
	0xD5, 0xD2, 0xB2, 0xBB, 0xB5, 0xBD, 0xD6, 0xB8, 0xB6, 0xA8,
	0xB5, 0xC4, 0xCE, 0xC4, 0xBC, 0xFE, 0xA1, 0xA3,
}

func TestANSIToUTF8DecodesGBK(t *testing.T) {
	if cp := ansiCodePage(); cp != 936 {
		t.Skipf("ANSI code page is %d, not 936 (GBK); fixture is CP936-specific", cp)
	}
	got := string(ansiToUTF8(gbkMissingTask))
	want := "错误: 系统找不到指定的文件。"
	if got != want {
		t.Fatalf("ansiToUTF8 decoded to %q, want %q", got, want)
	}
	// The decoded text must feed the idempotent-unregister match.
	if !isMissingTaskMsg(got) {
		t.Fatalf("decoded message not recognized as missing task: %q", got)
	}
}

func TestANSIToUTF8PassesASCII(t *testing.T) {
	in := []byte("ERROR: The system cannot find the file specified.\r\n")
	if got := string(ansiToUTF8(in)); got != string(in) {
		t.Fatalf("ASCII changed: %q", got)
	}
	if got := ansiToUTF8(nil); len(got) != 0 {
		t.Fatalf("nil input should stay empty, got %v", got)
	}
}
