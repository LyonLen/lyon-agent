//go:build !windows

package timer

// ansiToUTF8 is the identity off Windows: there is no schtasks to
// decode (the Backend dispatches it to crontab).
func ansiToUTF8(b []byte) []byte { return b }
