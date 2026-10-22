package market

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Acceptance tests for the ledger's durability contract (#54 hardening).
// They deliberately assert the behaviour the rest of this repo already
// honours (atomic replace, 0600 payloads, one shared in-process table),
// so a red here is a report for the implementation owner, not a nit.

// ledgerPath is the file Open/Record use, derived the same way.
func ledgerPath(t *testing.T) string {
	t.Helper()
	h := os.Getenv("LYON_HOME")
	if h == "" {
		t.Fatal("LYON_HOME unset")
	}
	return filepath.Join(h, "market.json")
}

// TestOpenReturnsSharedTable: Open is called once per finished prompt
// (TUI prompt_end, CLI fileMarket). If every call returns a fresh Table
// with its own mutex, two concurrent finishers each load the whole file,
// append their own row, and the last writer drops the other's record —
// silent ledger loss under exactly the load the ledger exists to record.
func TestOpenReturnsSharedTable(t *testing.T) {
	scratchHome(t)

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Exactly how production calls it: no handle is retained.
			Open().Record(Record{
				Class:  "coding",
				Prompt: "task",
				DurMS:  int64(1000 + i),
				TS:     time.Now(),
			})
		}(i)
	}
	wg.Wait()

	var onDisk Table
	raw, err := os.ReadFile(ledgerPath(t))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("decode ledger: %v", err)
	}
	if len(onDisk.Records) != writers {
		t.Errorf("ledger holds %d records after %d concurrent Open().Record() calls, "+
			"want %d — Open must return one shared in-process table, not a fresh "+
			"copy per call", len(onDisk.Records), writers, writers)
	}
}

// TestRecordDoesNotMutateInPlace: a crash mid-write must not destroy the
// ledger. Every other persistence path in this repo stages a temp file
// and renames it (memory/store.go atomicWrite, agent.SaveSnapshot,
// hooks.writeStatsFile). A bare WriteFile truncates the file first — pull
// the power at the wrong moment and months of history are gone, and
// Open() reports corrupt state as "no memory", so the loss is silent.
//
// Detection: hold an open handle on the existing ledger, write a new
// record, then read through the old handle. An atomic replace leaves the
// old inode alone (handle still sees the old bytes); an in-place write
// overwrites it underneath us.
func TestRecordDoesNotMutateInPlace(t *testing.T) {
	scratchHome(t)
	tab := Open()
	tab.Record(rec("coding", 1000, ""))

	path := ledgerPath(t)
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("read seeded ledger: %v", err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatalf("hold open ledger: %v", err)
	}
	defer held.Close()

	tab.Record(rec("coding", 2000, ""))

	// Windows: os.Rename refuses to replace an open file (sharing
	// violation — Go's os.Open shares read/write but not delete). That
	// refusal is itself the proof of non-in-place: a bare WriteFile
	// would sail through the held handle and change the bytes any
	// reader sees, while the atomic replace is blocked while the file
	// is open. Assert the file did NOT move under the handle, then
	// release it — the next Record rewrites the whole table and both
	// new rows must land.
	if os.PathSeparator == '\\' {
		mid, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read ledger under held handle: %v", err)
		}
		if strings.Contains(string(mid), "2000") {
			t.Error("Record wrote market.json in place — the file changed while a " +
				"handle held it open; it must stage a temp file and rename")
		}
		held.Close()
		tab.Record(rec("coding", 3000, "")) // flushes the whole table
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read ledger after flush: %v", err)
		}
		if !strings.Contains(string(after), "2000") || !strings.Contains(string(after), "3000") {
			t.Errorf("staged rows did not land after the handle closed: %s", after)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Error("Record left a .tmp file behind")
		}
		return
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated ledger: %v", err)
	}
	stale := make([]byte, len(after)+8)
	n, _ := held.ReadAt(stale, 0)
	stale = stale[:n]

	// Compare the overlap only: the file grew, so a whole-buffer compare
	// would differ under either strategy.
	overlap := len(stale)
	if overlap > len(after) {
		overlap = len(after)
	}
	if string(stale[:overlap]) == string(after[:overlap]) {
		t.Error("Record overwrote market.json in place; it must stage a temp file and " +
			"rename (see memory.Store / agent.SaveSnapshot), so a torn write cannot " +
			"truncate the ledger")
	}
	if !strings.Contains(string(after), "2000") {
		t.Errorf("updated ledger does not contain the new record: %s", after)
	}
}

// TestRecordFileMode: market.json stores prompt previews — the same class
// of payload agent/snapshot.go keeps at 0600. Group/world-readable means
// any local account can read what the user asked the agent.
func TestRecordFileMode(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX mode bits are not enforced on Windows")
	}
	scratchHome(t)
	tab := Open()
	tab.Record(rec("coding", 1000, ""))

	fi, err := os.Stat(ledgerPath(t))
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("market.json mode is %v; prompt previews must not be group/world "+
			"readable — snapshot.go writes 0600 for the same reason", fi.Mode().Perm())
	}
}

// TestCorruptLedgerRecoversCleanly: Open treats unreadable state as an
// empty ledger by design — that contract must survive a torn file, and a
// fresh Record must repair the file rather than append to garbage.
func TestCorruptLedgerRecoversCleanly(t *testing.T) {
	scratchHome(t)
	path := ledgerPath(t)
	if err := os.WriteFile(path, []byte(`{"records":[{"class":"coding","dur_`), 0o600); err != nil {
		t.Fatalf("seed corrupt ledger: %v", err)
	}

	tab := Open()
	if n := len(tab.Recent(10)); n != 0 {
		t.Fatalf("corrupt ledger decoded %d records, want 0", n)
	}
	tab.Record(rec("coding", 1000, ""))

	var after Table
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired ledger: %v", err)
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("ledger still corrupt after a clean Record: %v", err)
	}
	if len(after.Records) != 1 {
		t.Errorf("repaired ledger holds %d records, want 1", len(after.Records))
	}
}
