package agent

import (
	"os"
	"testing"
	"time"
)

// tempDir is t.TempDir with a deletion retry: these tests churn atomic
// tmp+rename writes (snapshots, memory), and on Windows the cleanup
// occasionally races a transient handle (defender/indexer) in %TEMP% —
// RemoveAll fails with "directory not empty" right after emptying it.
// The retry runs before testing's own cleanup (LIFO), turning the
// package-level flake green.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}
