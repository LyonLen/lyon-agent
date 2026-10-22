package market

import (
	"os"
	"testing"
	"time"
)

// scratchHome points LYON_HOME at a fresh temp dir with a retrying cleanup.
// t.TempDir's immediate RemoveAll fails on Windows often enough to be noise:
// a just-written ledger is still held (or delete-pending) when the framework
// walks the tree, and the test reports "directory not empty" even though
// every assertion passed. The same hardening lives in pkg/eval (scratchHome),
// pkg/flow (settle) and pkg/memory (scratchTempDir). No parallel tests in
// this package — the env is process-wide. Returns the dir.
func scratchHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		var err error
		for i := 0; i < 40; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("settle cleanup failed: %v", err)
	})
	t.Setenv("LYON_HOME", dir)
	return dir
}
