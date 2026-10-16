package timer

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSchtasksCreateArgsInterval(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		ms   int64
		want []string // the /SC tail
		bad  bool
	}{
		{"sub-minute rejected", 500, nil, true},
		{"non-whole minute rejected", 90_000, nil, true},
		{"15 minutes", 900_000, []string{"/SC", "MINUTE", "/MO", "15"}, false},
		{"2 hours", 7_200_000, []string{"/SC", "HOURLY", "/MO", "2"}, false},
		{"1 day", 86_400_000, []string{"/SC", "DAILY", "/MO", "1"}, false},
		{"2 days", 172_800_000, []string{"/SC", "DAILY", "/MO", "2"}, false},
		{"59 minutes stays MINUTE", 3_540_000, []string{"/SC", "MINUTE", "/MO", "59"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk := &Task{ID: "t", Kind: KindInterval, EveryMS: c.ms, Prompt: "x", Enabled: true}
			args, err := SchtasksCreateArgs(tk, `C:\lyon\lyon.exe`, now)
			if c.bad {
				if err == nil {
					t.Fatalf("want error, got %v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			joined := strings.Join(args, " ")
			for i := 0; i < len(c.want); i += 2 {
				if !strings.Contains(joined, c.want[i]+" "+c.want[i+1]) {
					t.Fatalf("args %v missing %s %s", args, c.want[i], c.want[i+1])
				}
			}
			// Common shape: /Create /TN lyon-t /TR ... /F + schedule.
			if args[0] != "/Create" || args[1] != "/TN" || args[2] != "lyon-t" ||
				args[3] != "/TR" || args[5] != "/F" {
				t.Fatalf("bad base shape: %v", args)
			}
		})
	}
}

func TestSchtasksCreateArgsCronShapes(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.Local)
	cases := []struct {
		cron string
		want string
		bad  bool
	}{
		{"30 8 * * *", "/SC DAILY /ST 08:30", false},
		{"*/10 * * * *", "/SC MINUTE /MO 10", false},
		{"0 9 * * MON-FRI", "/SC WEEKLY /D MON,TUE,WED,THU,FRI /ST 09:00", false},
		{"15 3 * * 1", "/SC WEEKLY /D MON /ST 03:15", false},
		{"0 0 15 * *", "/SC MONTHLY /D 15 /ST 00:00", false},
		{"0,30 9,17 * * *", "", true}, // list hours — reject clearly
		{"0 0 30 2 *", "", true},      // impossible date — reject
	}
	for _, c := range cases {
		t.Run(c.cron, func(t *testing.T) {
			tk := &Task{ID: "t", Kind: KindCron, Cron: c.cron, Prompt: "x", Enabled: true}
			args, err := SchtasksCreateArgs(tk, `C:\lyon\lyon.exe`, now)
			if c.bad {
				if err == nil {
					t.Fatalf("want error, got %v", args)
				}
				if !strings.Contains(err.Error(), "in-process") && !strings.Contains(err.Error(), "never matches") &&
					!strings.Contains(err.Error(), "no valid weekday") {
					t.Fatalf("error should point at in-process fallback: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, c.want) {
				t.Fatalf("args %q want substring %q", joined, c.want)
			}
		})
	}
}

func TestSchtasksTaskRunQuoting(t *testing.T) {
	cases := []struct {
		name string
		tk   *Task
		exe  string
		want string
	}{
		{
			"chinese prompt with space",
			&Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Prompt: "每日 报告"},
			`C:\Program Files\lyon\lyon.exe`,
			`"C:\Program Files\lyon\lyon.exe" run -- "每日 报告"`,
		},
		{
			"embedded quotes escaped",
			&Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Prompt: `say "hi"`},
			`C:\lyon\lyon.exe`,
			`C:\lyon\lyon.exe run -- "say \"hi\""`,
		},
		{
			"workspace flag",
			&Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Prompt: "x", Workspace: `D:\my work`},
			`C:\lyon\lyon.exe`,
			`C:\lyon\lyon.exe run -workspace "D:\my work" -- x`,
		},
		{
			// lyon has no /C verb: the action must be cmd.exe itself,
			// or registration succeeds while every fire fails.
			"command task goes through cmd.exe",
			&Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Command: "echo hello && dir"},
			`C:\lyon\lyon.exe`,
			`C:\Windows\system32\cmd.exe /C "echo hello && dir"`,
		},
		{
			"command with quotes reaches cmd verbatim",
			&Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Command: `echo "hi" > out.txt`},
			`C:\lyon\lyon.exe`,
			`C:\Windows\system32\cmd.exe /C "echo "hi" > out.txt"`,
		},
		{
			"command with spaces in path quoted",
			&Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Command: `dir "C:\Program Files"`},
			`C:\lyon\lyon.exe`,
			`C:\Windows\system32\cmd.exe /C "dir "C:\Program Files""`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("COMSPEC", `C:\Windows\system32\cmd.exe`)
			got, err := SchtasksTaskRun(c.tk, c.exe)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestWinQuoteArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`C:\no\space`, `C:\no\space`},
		{`has space`, `"has space"`},
		{`say "hi"`, `"say \"hi\""`},
		{`end\`, `end\`},                  // bare, no space
		{`with space\`, `"with space\\"`}, // trailing backslash doubled
		{`a\"b`, `"a\\\"b"`},              // backslash before quote doubled+escaped
		{"", `""`},
	}
	for _, c := range cases {
		if got := winQuoteArg(c.in); got != c.want {
			t.Fatalf("winQuoteArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSchtasksOnceArgs(t *testing.T) {
	tk := &Task{ID: "t", Kind: KindOnce, At: at("2026-10-16T09:05:00+08:00"), Prompt: "x", Enabled: true}
	args, err := SchtasksCreateArgs(tk, `C:\lyon\lyon.exe`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/SC ONCE /ST 09:05 /SD 2026/10/16") {
		t.Fatalf("once schedule wrong: %s", joined)
	}
}

func TestSchtasksDeleteArgs(t *testing.T) {
	got := strings.Join(SchtasksDeleteArgs("daily"), " ")
	if got != "/Delete /TN lyon-daily /F" {
		t.Fatalf("delete args: %s", got)
	}
}

func TestParseSchtasksQuery(t *testing.T) {
	out := "\"TaskName\",\"Next Run Time\",\"Status\"\r\n" +
		"\"\\lyon-daily\",\"2026/10/16 08:00:00\",\"Ready\"\r\n" +
		"\"\\Microsoft\\Windows\\defrag\",\"...\",\"Ready\"\r\n" +
		"\"\\lyon-nightly\",\"...\",\"Ready\"\r\n"
	ids := ParseSchtasksQuery(out)
	if len(ids) != 2 || ids[0] != "daily" || ids[1] != "nightly" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestCrontabLine(t *testing.T) {
	logs := `C:\Users\me\.lyon\logs`
	cases := []struct {
		name string
		tk   *Task
		bad  bool
		want []string // substrings that must all appear
	}{
		{
			"daily cron with anchor and log redirect",
			&Task{ID: "daily", Kind: KindCron, Cron: "30 8 * * 1-5", Prompt: "生成日报"},
			false,
			[]string{
				"30 8 * * 1-5",
				" run ",
				`'生成日报'`,
				" >> ",
				// printf-built %F: the line itself carries no %, so
				// Vixie cron's percent-to-newline rule can never
				// mangle the redirect.
				"$(date +$(printf '\045F'))",
				"2>&1",
				"# lyon:daily",
			},
		},
		{
			"interval divisor of 60",
			&Task{ID: "p", Kind: KindInterval, EveryMS: 300_000, Prompt: "x"},
			false,
			[]string{"*/5 * * * *", "# lyon:p"},
		},
		{
			"hourly interval",
			&Task{ID: "p", Kind: KindInterval, EveryMS: 3_600_000, Prompt: "x"},
			false,
			[]string{"0 */1 * * *"},
		},
		{
			"non-divisor interval rejected",
			&Task{ID: "p", Kind: KindInterval, EveryMS: 420_000, Prompt: "x"},
			true, nil,
		},
		{
			// The OS-level floor: crontab is minute-grained, so a
			// sub-minute interval must be refused here too (matching
			// schtasksSchedule), never silently rounded.
			"sub-minute interval rejected",
			&Task{ID: "p", Kind: KindInterval, EveryMS: 500, Prompt: "x"},
			true, nil,
		},
		{
			"once rejected with in-process hint",
			&Task{ID: "p", Kind: KindOnce, At: at("2026-10-16T09:00:00Z"), Prompt: "x"},
			true, nil,
		},
		{
			"command task verbatim",
			&Task{ID: "c", Kind: KindCron, Cron: "0 3 * * *", Command: "make backup"},
			false,
			[]string{"0 3 * * * make backup", "# lyon:c"},
		},
		{
			// Vixie cron turns a bare % into a newline; the payload's
			// percents must carry the documented backslash escape.
			"command percent escaped for cron",
			&Task{ID: "p", Kind: KindCron, Cron: "0 4 * * *", Command: "df -h | grep 80%"},
			false,
			[]string{`df -h | grep 80\%`},
		},
		{
			"prompt percent escaped for cron",
			&Task{ID: "p", Kind: KindCron, Cron: "0 9 * * *", Prompt: "检查80%磁盘"},
			false,
			[]string{`'检查80\%磁盘'`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, err := CrontabLine(c.tk, `C:\lyon\lyon.exe`, logs)
			if c.bad {
				if err == nil {
					t.Fatalf("want error, got line %q", line)
				}
				if c.name == "once rejected with in-process hint" &&
					!strings.Contains(err.Error(), "in-process") {
					t.Fatalf("error should point at in-process: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			for _, w := range c.want {
				if !strings.Contains(line, w) {
					t.Fatalf("line %q missing %q", line, w)
				}
			}
			if strings.Count(line, "\n") != 0 {
				t.Fatalf("line must be single: %q", line)
			}
		})
	}
}

func TestCrontabLinePromptEscaping(t *testing.T) {
	tk := &Task{ID: "q", Kind: KindCron, Cron: "0 9 * * *", Prompt: "it's a 'test'"}
	line, err := CrontabLine(tk, "/usr/bin/lyon", "/home/me/.lyon/logs")
	if err != nil {
		t.Fatal(err)
	}
	// sh-escaping: ' -> '\'' so quoting never breaks.
	if !strings.Contains(line, `'it'\''s a '\''test'\''"`) &&
		!strings.Contains(line, `run -- 'it'\''s a '\''test'\''`) {
		t.Fatalf("prompt not sh-escaped: %s", line)
	}
}

func TestCrontabUpsertIdempotent(t *testing.T) {
	tk := &Task{ID: "daily", Kind: KindCron, Cron: "30 8 * * *", Prompt: "x"}
	line, err := CrontabLine(tk, "/usr/bin/lyon", "/home/me/.lyon/logs")
	if err != nil {
		t.Fatal(err)
	}
	existing := "# my own crontab\n0 5 * * * /usr/bin/backup\n"
	once := CrontabUpsert(existing, line, "daily")
	if !strings.Contains(once, "# my own crontab") || !strings.Contains(once, "/usr/bin/backup") {
		t.Fatalf("user crontab damaged:\n%s", once)
	}
	if !strings.Contains(once, "# lyon:daily") {
		t.Fatalf("anchor missing:\n%s", once)
	}
	// Re-register with a changed schedule: exactly one lyon:daily line,
	// the new one.
	tk.Cron = "0 6 * * *"
	line2, _ := CrontabLine(tk, "/usr/bin/lyon", "/home/me/.lyon/logs")
	twice := CrontabUpsert(once, line2, "daily")
	if n := strings.Count(twice, "# lyon:daily"); n != 1 {
		t.Fatalf("want exactly one lyon:daily line, got %d:\n%s", n, twice)
	}
	if !strings.Contains(twice, "0 6 * * *") || strings.Contains(twice, "30 8 * * *") {
		t.Fatalf("stale schedule survived:\n%s", twice)
	}
	// Another task's line must survive the upsert.
	tk2 := &Task{ID: "nightly", Kind: KindCron, Cron: "0 22 * * *", Prompt: "y"}
	line3, _ := CrontabLine(tk2, "/usr/bin/lyon", "/home/me/.lyon/logs")
	both := CrontabUpsert(twice, line3, "nightly")
	after := CrontabUpsert(both, line2, "daily")
	if !strings.Contains(after, "# lyon:nightly") {
		t.Fatalf("other task's line lost:\n%s", after)
	}
	if strings.Count(after, "# lyon:daily") != 1 {
		t.Fatalf("daily line duplicated:\n%s", after)
	}
}

func TestCrontabRemoveOnlyOwnLine(t *testing.T) {
	tk := &Task{ID: "daily", Kind: KindCron, Cron: "30 8 * * *", Prompt: "x"}
	line, _ := CrontabLine(tk, "/usr/bin/lyon", "/home/me/.lyon/logs")
	tk2 := &Task{ID: "nightly", Kind: KindCron, Cron: "0 22 * * *", Prompt: "y"}
	line2, _ := CrontabLine(tk2, "/usr/bin/lyon", "/home/me/.lyon/logs")
	content := "# user line\n" + line + "\n" + line2
	out := CrontabRemove(content, "daily")
	if strings.Contains(out, "# lyon:daily") {
		t.Fatalf("daily not removed:\n%s", out)
	}
	if !strings.Contains(out, "# lyon:nightly") || !strings.Contains(out, "# user line") {
		t.Fatalf(" collateral damage:\n%s", out)
	}
}

func TestParseCrontabLyonIDs(t *testing.T) {
	content := "# user stuff\n0 5 * * * backup\n" +
		"30 8 * * * lyon run -- 'a' >> '/x/logs/timer-os-'$(date +$(printf '\045F'))'.log' 2>&1 # lyon:daily\n" +
		"0 22 * * * lyon run -- 'b' >> '/x/logs/timer-os-'$(date +$(printf '\045F'))'.log' 2>&1 # lyon:nightly\n"
	ids := ParseCrontabLyonIDs(content)
	if len(ids) != 2 || ids[0] != "daily" || ids[1] != "nightly" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestShQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"has space", "'has space'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shQuote(c.in); got != c.want {
			t.Fatalf("shQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestIsMissingTaskMsg(t *testing.T) {
	// Input is ANSI-decoded text (ansiToUTF8) before matching.
	missing := []string{
		"ERROR: The system cannot find the file specified.",
		"错误: 系统找不到指定的文件。",
		"错误: 找不到 *。", // truncated localized variants still hit 找不到
	}
	for _, m := range missing {
		if !isMissingTaskMsg(m) {
			t.Fatalf("should be missing: %q", m)
		}
	}
	present := []string{
		"",
		"ERROR: Access is denied.",
		"错误: 拒绝访问。",
		"SUCCESS: The scheduled task \"lyon-x\" has successfully been created.",
	}
	for _, m := range present {
		if isMissingTaskMsg(m) {
			t.Fatalf("should NOT be missing: %q", m)
		}
	}
}

// TestBackendRegisterIntegration really registers/unregisters an OS
// task. Skipped by default; run with LYON_TIMER_ITEST=1 on a machine
// where you accept a real (briefly existing) scheduled task.
func TestBackendRegisterIntegration(t *testing.T) {
	if testing.Short() || getenv("LYON_TIMER_ITEST") == "" {
		t.Skip("set LYON_TIMER_ITEST=1 to run real OS registration")
	}
	scratchHome(t)
	b := NewBackend()
	tk := &Task{ID: "itest-probe", Kind: KindInterval, EveryMS: 3_600_000,
		Prompt: "integration probe", Enabled: true}
	if err := b.Register(tk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer b.Unregister(tk.ID)
	ids, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == tk.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("registered task not listed: %v", ids)
	}
	if err := b.Unregister(tk.ID); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	// Unregister twice: idempotent.
	if err := b.Unregister(tk.ID); err != nil {
		t.Fatalf("Unregister again: %v", err)
	}
}

func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }
