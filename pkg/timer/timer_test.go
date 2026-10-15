package timer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchHome points LYON_HOME at a fresh temp dir with the repo's
// standard retry-cleanup (external %TEMP% scanners on Windows drop
// marker dirs mid-test, so a bare RemoveAll flakes).
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

func at(s string) *time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("bad time " + s)
	}
	return &tm
}

// readTodayLog returns today's timer log file content from home.
func readTodayLog(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, "logs", "timer-"+time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	return string(data)
}

func okTask(id string) *Task {
	return &Task{
		ID:      id,
		Kind:    KindInterval,
		EveryMS: 3600_000,
		Prompt:  "daily standup",
		Enabled: true,
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	scratchHome(t)
	in := &Task{
		ID:        "daily-report",
		Kind:      KindCron,
		Cron:      "30 8 * * 1-5",
		Prompt:    "生成日报：检查 git log 并总结",
		Workspace: `D:\work`,
		Enabled:   true,
		Overlap:   OverlapQueue,
		Miss:      MissOnce,
		TimeoutMS: 90_000,
		At:        at("2026-10-16T09:00:00+08:00"),
	}
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load("daily-report")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.ID != in.ID || out.Kind != in.Kind || out.Cron != in.Cron ||
		out.Prompt != in.Prompt || out.Workspace != in.Workspace ||
		out.Enabled != in.Enabled || out.Overlap != in.Overlap ||
		out.Miss != in.Miss || out.TimeoutMS != in.TimeoutMS {
		t.Fatalf("roundtrip mismatch:\n in=%+v\nout=%+v", in, out)
	}
	if out.At == nil || !out.At.Equal(*in.At) {
		t.Fatalf("at mismatch: %v vs %v", out.At, in.At)
	}
}

func TestLoadAllSkipsCorruptFile(t *testing.T) {
	home := scratchHome(t)
	dir := filepath.Join(home, "timers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Save(okTask("good")); err != nil {
		t.Fatal(err)
	}
	// Corrupt: invalid JSON. And a valid-JSON-but-bogus-kind file.
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "weird.json"), []byte(`{"id":"weird","kind":"wat"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks, err := LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "good" {
		t.Fatalf("want only [good], got %v", tasks)
	}
	// The corrupt file must also show up in the timer log.
	log := readTodayLog(t, home)
	if !strings.Contains(log, "skipping corrupt file broken.json") {
		t.Fatalf("corrupt skip not logged:\n%s", log)
	}
}

func TestLoadAllEmptyDir(t *testing.T) {
	scratchHome(t)
	tasks, err := LoadAll()
	if err != nil || tasks != nil {
		t.Fatalf("want nil,nil on empty store, got %v,%v", tasks, err)
	}
}

func TestDeleteMissingIsOK(t *testing.T) {
	scratchHome(t)
	if err := Delete("nope"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		t    *Task
		bad  bool
	}{
		{"interval ok", okTask("a"), false},
		{"bad id", &Task{ID: "../evil", Kind: KindInterval, EveryMS: 1000, Prompt: "x"}, true},
		{"id with space", &Task{ID: "a b", Kind: KindInterval, EveryMS: 1000, Prompt: "x"}, true},
		{"once without at", &Task{ID: "a", Kind: KindOnce, Prompt: "x"}, true},
		{"interval without every", &Task{ID: "a", Kind: KindInterval, Prompt: "x"}, true},
		{"cron empty", &Task{ID: "a", Kind: KindCron}, true},
		{"cron bad field count", &Task{ID: "a", Kind: KindCron, Cron: "* * *"}, true},
		{"cron bad value", &Task{ID: "a", Kind: KindCron, Cron: "99 * * * *"}, true},
		{"both payloads", &Task{ID: "a", Kind: KindInterval, EveryMS: 1000, Prompt: "x", Command: "ls"}, true},
		{"no payload", &Task{ID: "a", Kind: KindInterval, EveryMS: 1000}, true},
		{"multiline prompt", &Task{ID: "a", Kind: KindInterval, EveryMS: 1000, Prompt: "a\nb"}, true},
		{"bad overlap", &Task{ID: "a", Kind: KindInterval, EveryMS: 1000, Prompt: "x", Overlap: "maybe"}, true},
		{"bad miss", &Task{ID: "a", Kind: KindInterval, EveryMS: 1000, Prompt: "x", Miss: "maybe"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.t.Validate()
			if c.bad && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !c.bad && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
		})
	}
}

func TestNext(t *testing.T) {
	from := time.Date(2026, 10, 15, 10, 0, 0, 0, time.Local)

	t.Run("interval", func(t *testing.T) {
		tk := okTask("i")
		tk.EveryMS = 90_000
		next, err := tk.Next(from)
		if err != nil || !next.Equal(from.Add(90*time.Second)) {
			t.Fatalf("want +90s, got %v,%v", next, err)
		}
	})

	t.Run("once future", func(t *testing.T) {
		tk := &Task{ID: "o", Kind: KindOnce, At: at("2026-10-16T09:00:00+08:00"), Prompt: "x"}
		if _, err := tk.Next(from); err != nil {
			t.Fatalf("future once: %v", err)
		}
	})

	t.Run("once past is spent", func(t *testing.T) {
		tk := &Task{ID: "o", Kind: KindOnce, At: at("2020-01-01T00:00:00Z"), Prompt: "x"}
		if _, err := tk.Next(from); err != ErrNoNext {
			t.Fatalf("want ErrNoNext, got %v", err)
		}
	})

	t.Run("cron daily", func(t *testing.T) {
		tk := &Task{ID: "c", Kind: KindCron, Cron: "30 8 * * *", Prompt: "x"}
		next, _ := tk.Next(from)
		want := time.Date(2026, 10, 16, 8, 30, 0, 0, time.Local)
		if !next.Equal(want) {
			t.Fatalf("want %v, got %v", want, next)
		}
	})

	t.Run("cron step", func(t *testing.T) {
		tk := &Task{ID: "c", Kind: KindCron, Cron: "*/15 * * * *", Prompt: "x"}
		next, _ := tk.Next(from)
		want := time.Date(2026, 10, 15, 10, 15, 0, 0, time.Local)
		if !next.Equal(want) {
			t.Fatalf("want %v, got %v", want, next)
		}
	})

	t.Run("cron weekday names and ranges", func(t *testing.T) {
		// 2026-10-15 is a Thursday. Next MON-FRI 09:00 is tomorrow.
		tk := &Task{ID: "c", Kind: KindCron, Cron: "0 9 * * MON-FRI", Prompt: "x"}
		next, _ := tk.Next(from)
		want := time.Date(2026, 10, 16, 9, 0, 0, 0, time.Local)
		if !next.Equal(want) {
			t.Fatalf("want %v, got %v", want, next)
		}
	})

	t.Run("cron sunday via 7", func(t *testing.T) {
		// 7 == Sunday; 2026-10-18 is a Sunday.
		tk := &Task{ID: "c", Kind: KindCron, Cron: "0 0 * * 7", Prompt: "x"}
		next, _ := tk.Next(from)
		want := time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local)
		if !next.Equal(want) {
			t.Fatalf("want %v, got %v", want, next)
		}
	})

	t.Run("cron day-of-month or weekday OR semantics", func(t *testing.T) {
		// dom=1 or Saturday. 2026-10-17 is a Saturday, before the 1st
		// of next month — must fire Saturday, not wait for Nov 1.
		tk := &Task{ID: "c", Kind: KindCron, Cron: "0 0 1 * SAT", Prompt: "x"}
		next, _ := tk.Next(from)
		want := time.Date(2026, 10, 17, 0, 0, 0, 0, time.Local)
		if !next.Equal(want) {
			t.Fatalf("want %v, got %v", want, next)
		}
	})

	t.Run("cron impossible date never fires", func(t *testing.T) {
		tk := &Task{ID: "c", Kind: KindCron, Cron: "0 0 30 2 *", Prompt: "x"}
		if _, err := tk.Next(from); err != ErrNoNext {
			t.Fatalf("want ErrNoNext for Feb 30, got %v", err)
		}
	})
}
