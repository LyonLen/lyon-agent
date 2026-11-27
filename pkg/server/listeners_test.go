package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"lyon-agent/pkg/agent"
)

// TestParseNetstatTCP pins the Windows wire format: only LISTENING rows
// survive, the OS band (135) and the RPC/ephemeral tail (49664) are
// dropped, non-listening states are dropped, and the [v6]:port column
// still yields a usable address.
func TestParseNetstatTCP(t *testing.T) {
	out := "Active Connections\r\n" +
		"\r\n" +
		"  Proto  Local Address          Foreign Address        State           PID\r\n" +
		"  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1356\r\n" +
		"  TCP    127.0.0.1:5183         0.0.0.0:0              LISTENING       26988\r\n" +
		"  TCP    [::1]:5183             [::]:0                 LISTENING       26988\r\n" +
		"  TCP    127.0.0.1:8081         0.0.0.0:0              LISTENING       43848\r\n" +
		"  TCP    127.0.0.1:49664        0.0.0.0:0              LISTENING       972\r\n" +
		"  TCP    127.0.0.1:8080         0.0.0.0:0              ESTABLISHED     51404\r\n" +
		"  UDP    0.0.0.0:5040           *:*                                    7904\r\n"

	rows := parseNetstatTCP(out)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	want := []portRow{
		{addr: "127.0.0.1", port: 5183, pid: 26988},
		{addr: "::1", port: 5183, pid: 26988},
		{addr: "127.0.0.1", port: 8081, pid: 43848},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

// TestGroupListeners: one process is one row — the IPv4 and IPv6 binds of
// the same pid collapse, ports sort ascending, unknown pids (0) stand
// alone, and rows order by their lowest port.
func TestGroupListeners(t *testing.T) {
	raw := []portRow{
		{addr: "::1", port: 5183, pid: 2185, name: "vite"},
		{addr: "127.0.0.1", port: 5183, pid: 2185, name: "vite"},
		{addr: "127.0.0.1", port: 14016, pid: 32888, name: "Weixin.exe"},
		{addr: "127.0.0.1", port: 14013, pid: 32888, name: "Weixin.exe"},
		{addr: "127.0.0.1", port: 8091, pid: 13208, name: "proxy-build.exe"},
		{addr: "0.0.0.0", port: 8088, pid: 0},
	}
	got := groupListeners(raw, map[int]bool{2185: true})
	want := []listenerInfo{
		{PID: 2185, Name: "vite", Ports: []int{5183}, Managed: true},
		{PID: 0, Name: "", Ports: []int{8088}, Managed: false},
		{PID: 13208, Name: "proxy-build.exe", Ports: []int{8091}, Managed: false},
		{PID: 32888, Name: "Weixin.exe", Ports: []int{14013, 14016}, Managed: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grouped = %+v\nwant %+v", got, want)
	}
}

// TestParseTasklistCSV: the name column is best-effort decoration, so the
// only contract is pid→name from the CSV layout tasklist actually prints.
func TestParseTasklistCSV(t *testing.T) {
	out := "\"lyond.exe\",\"43848\",\"Console\",\"1\",\"27,000 K\"\r\n" +
		"\"System Idle Process\",\"0\",\"Services\",\"0\",\"8 K\"\r\n" +
		"INFO: no tasks\r\n"
	names := parseTasklistCSV(out)
	if names[43848] != "lyond.exe" {
		t.Errorf("pid 43848 = %q, want lyond.exe", names[43848])
	}
	if names[0] != "System Idle Process" {
		t.Errorf("pid 0 = %q", names[0])
	}
	if len(names) != 2 {
		t.Errorf("got %d entries, want 2 (the INFO line is not a task)", len(names))
	}
}

// TestParseSS: iproute2 layout with the pid buried in users:(("…",pid=N)).
func TestParseSS(t *testing.T) {
	out := "LISTEN 0 128 127.0.0.1:8080 0.0.0.0:* users:((\"lyond\",pid=123,fd=6))\n" +
		"LISTEN 0 128 [::1]:5183 [::]:* users:((\"node\",pid=456,fd=20))\n" +
		"LISTEN 0 128 0.0.0.0:8088 0.0.0.0:*\n"
	rows := parseSS(out)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	if rows[0].port != 8080 || rows[0].pid != 123 || rows[0].name != "lyond" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].addr != "::1" || rows[1].port != 5183 || rows[1].pid != 456 {
		t.Errorf("row 1 = %+v (bracketed v6 address)", rows[1])
	}
	// Hidden pid (another user's listener): the row still counts, name/pid blank.
	if rows[2].port != 8088 || rows[2].pid != 0 || rows[2].name != "" {
		t.Errorf("row 2 = %+v", rows[2])
	}
}

// TestParseNetstatLinux: the net-tools fallback carries pid/name as the
// last column, not as users:(…).
func TestParseNetstatLinux(t *testing.T) {
	out := "Active Internet connections (only servers)\n" +
		"Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name\n" +
		"tcp        0      0 127.0.0.1:8080          0.0.0.0:*               LISTEN      1234/lyond\n" +
		"tcp6       0      0 :::5183                 :::*                    LISTEN      456/node\n"
	rows := parseNetstatLinux(out)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].pid != 1234 || rows[0].name != "lyond" || rows[0].port != 8080 {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].port != 5183 || rows[1].pid != 456 {
		t.Errorf("row 1 = %+v", rows[1])
	}
}

// TestListenersEndpoint: the route answers JSON with a listeners array
// even where the host tool is missing (there the array is empty and
// error says why) — the panel renders either.
func TestListenersEndpoint(t *testing.T) {
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	srv := newHTTPTestServer(t, s)

	code, body := srv.get("/v1/listeners")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var doc struct {
		Listeners []listenerInfo `json:"listeners"`
		Error     string         `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if doc.Listeners == nil {
		t.Errorf("listeners should be [] not null: %s", body)
	}
	for _, l := range doc.Listeners {
		if len(l.Ports) == 0 {
			t.Errorf("row without ports: %+v", l)
		}
		for _, p := range l.Ports {
			if p < minInterestingPort || p > maxInterestingPort {
				t.Errorf("port %d outside the userland band", p)
			}
		}
	}
	if len(doc.Listeners) > listenersCap {
		t.Errorf("%d rows over the cap %d", len(doc.Listeners), listenersCap)
	}
}
