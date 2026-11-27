package server

// GET /v1/listeners — the process panel's fallback view.
//
// ListProcesses only knows tasks the model started through
// run_shell background=true: a service a human (or an older session) left
// running with nohup never enters that registry, so a panel showing "0"
// while three servers are up is misleading. This route answers the other
// half of the question — what is actually listening on this box — with
// the three fields a dev needs to connect the dots: ports, pid, process
// name. Ports are grouped per process: this box's five Weixin listeners
// are one row, not five.
//
// Deliberately not a proto route: the registry RPCs are a stable wire
// contract, this is a host-observation convenience, and adding it as
// `GET /v1/listeners` reuses the /v1 JSON seam (and its vite proxy)
// without a buf regenerate.
//
// Scope: ports 1024-49151 only. Below 1024 is the OS (135/445/3389), above
// is the Windows RPC/ephemeral range (49664+ svchost stubs) — neither
// helps a dev wiring a service, and both would bury the interesting rows.

import (
	"context"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"lyon-agent/pkg/tools"
)

const (
	// minInterestingPort / maxInterestingPort bound what the fallback
	// table shows: the userland service band, minus OS ports and the
	// ephemeral/RPC tail.
	minInterestingPort = 1024
	maxInterestingPort = 49151
	// listenersCap bounds one response — the panel is a glance, not a
	// netstat dump.
	listenersCap = 20
)

// listenerInfo is one row of the panel's fallback: a process and the
// ports it listens on. Managed is true when the pid belongs to a
// run_shell background=true task, i.e. the registry view and the host
// view describe the same process.
type listenerInfo struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	Ports   []int  `json:"ports"`
	Managed bool   `json:"managed"`
}

// portRow is one raw listener line before grouping (IPv4 and IPv6 binds of
// the same pid/port collapse into a single listenerInfo).
type portRow struct {
	addr string
	port int
	pid  int
	name string
}

func (s *Server) handleListeners(w http.ResponseWriter, _ *http.Request) {
	rows, err := listeningPorts()
	body := map[string]any{"listeners": rows}
	if err != nil {
		// The host command may not exist (locked-down CI, odd container):
		// answer 200 with the reason so the panel renders an explanation
		// instead of an error banner.
		body["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// listeningPorts enumerates host TCP listeners, groups them per process,
// and caps the result. Errors are advisory: an empty list plus the reason
// beats a 500.
func listeningPorts() ([]listenerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var raw []portRow
	var err error
	if runtime.GOOS == "windows" {
		raw, err = windowsListeners(ctx)
	} else {
		raw, err = unixListeners(ctx)
	}
	if err != nil {
		return []listenerInfo{}, err
	}

	// Group by pid so one process is one row; a hidden pid (another user's
	// listener, which the kernel won't name) stands alone keyed by port.
	out := groupListeners(raw, managedPIDs())
	if len(out) > listenersCap {
		out = out[:listenersCap]
	}
	return out, nil
}

// groupListeners collapses raw listener lines into one row per process:
// the IPv4 and IPv6 binds of the same service are one entry, ports sorted
// ascending, rows ordered by their lowest port.
func groupListeners(raw []portRow, managed map[int]bool) []listenerInfo {
	byKey := make(map[int]*listenerInfo, len(raw))
	keys := make([]int, 0, len(raw))
	for _, r := range raw {
		key := r.pid
		if key <= 0 {
			key = -r.port
		}
		row, ok := byKey[key]
		if !ok {
			row = &listenerInfo{PID: r.pid, Name: r.name}
			byKey[key] = row
			keys = append(keys, key)
		}
		if row.Name == "" {
			row.Name = r.name
		}
		if !containsPort(row.Ports, r.port) {
			row.Ports = append(row.Ports, r.port)
		}
	}

	out := make([]listenerInfo, 0, len(keys))
	for _, k := range keys {
		row := byKey[k]
		sort.Ints(row.Ports)
		row.Managed = managed[row.PID]
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ports[0] != out[j].Ports[0] {
			return out[i].Ports[0] < out[j].Ports[0]
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// managedPIDs is the pid set the background registry already knows, so a
// port row can say "this one is mine" instead of "非托管".
func managedPIDs() map[int]bool {
	m := make(map[int]bool)
	for _, b := range tools.ListBackground() {
		m[b.PID] = true
	}
	return m
}

func windowsListeners(ctx context.Context) ([]portRow, error) {
	// No -p TCP filter: that narrowed the dump to IPv4 and silently dropped
	// IPv6 listeners — including vite, which binds ::1 only. UDP rows carry
	// no state column, so parseNetstatTCP skips them on field count.
	out, err := exec.CommandContext(ctx, "netstat", "-ano").Output()
	if err != nil {
		return nil, err
	}
	rows := parseNetstatTCP(string(out))
	// Process names are a second call; a miss only costs the name column.
	if names := windowsProcessNames(ctx); len(names) > 0 {
		for i := range rows {
			rows[i].name = names[rows[i].pid]
		}
	}
	return rows, nil
}

// parseNetstatTCP reads `netstat -ano` output:
//
//	TCP    127.0.0.1:8081         0.0.0.0:0              LISTENING       43848
//	TCP    [::1]:5183             [::]:0                 LISTENING       26988
//
// Pure function so the wire format is pinned by a test rather than by
// whatever this box happens to print.
func parseNetstatTCP(out string) []portRow {
	var rows []portRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimRight(line, "\r"))
		if len(f) < 5 || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		addr, port, ok := splitHostPort(f[1])
		if !ok || port < minInterestingPort || port > maxInterestingPort {
			continue
		}
		pid, err := strconv.Atoi(f[4])
		if err != nil {
			continue
		}
		rows = append(rows, portRow{addr: addr, port: port, pid: pid})
	}
	return rows
}

// splitHostPort splits the local-address column, which Windows prints as
// host:port and as [v6]:port for IPv6.
func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(s[i+1:]))
	if err != nil {
		return "", 0, false
	}
	return strings.Trim(s[:i], "[]"), port, true
}

func windowsProcessNames(ctx context.Context) map[int]string {
	out, err := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil
	}
	return parseTasklistCSV(string(out))
}

// parseTasklistCSV reads `tasklist /FO CSV /NH`:
//
//	"lyond.exe","43848","Console","1","27,000 K"
func parseTasklistCSV(out string) map[int]string {
	m := make(map[int]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		parts := strings.Split(line, `","`)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimPrefix(parts[0], `"`)
		pid, err := strconv.Atoi(strings.Trim(parts[1], `"`))
		if err != nil {
			continue
		}
		m[pid] = name
	}
	return m
}

func unixListeners(ctx context.Context) ([]portRow, error) {
	// ss ships with iproute2; fall back to netstat on older images.
	if out, err := exec.CommandContext(ctx, "ss", "-ltnpH").Output(); err == nil {
		return parseSS(string(out)), nil
	}
	out, err := exec.CommandContext(ctx, "netstat", "-ltnp").Output()
	if err != nil {
		return nil, err
	}
	return parseNetstatLinux(string(out)), nil
}

// parseSS reads `ss -ltnH` lines (the -H header drop keeps the state
// column; the pid/name only appear with -p, and only for processes this
// user may inspect, so they stay empty when the kernel hides them):
//
//	LISTEN 0 128 127.0.0.1:8080 0.0.0.0:* users:(("lyond",pid=123,fd=6))
func parseSS(out string) []portRow {
	var rows []portRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimRight(line, "\r"))
		if len(f) < 5 || !strings.EqualFold(f[0], "LISTEN") {
			continue
		}
		addr, port, ok := splitHostPort(f[3])
		if !ok || port < minInterestingPort || port > maxInterestingPort {
			continue
		}
		row := portRow{addr: addr, port: port}
		if i := strings.Index(line, `users:(("`); i >= 0 {
			rest := line[i+len(`users:(("`):]
			if j := strings.Index(rest, `"`); j > 0 {
				row.name = rest[:j]
			}
			if j := strings.Index(rest, "pid="); j >= 0 {
				num := rest[j+len("pid="):]
				if k := strings.IndexAny(num, ",)"); k > 0 {
					num = num[:k]
				}
				if pid, err := strconv.Atoi(num); err == nil {
					row.pid = pid
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// parseNetstatLinux reads the net-tools fallback, whose pid rides the
// last column as "pid/name":
//
//	tcp  0  0 127.0.0.1:8080 0.0.0.0:* LISTEN 1234/lyond
func parseNetstatLinux(out string) []portRow {
	var rows []portRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimRight(line, "\r"))
		if len(f) < 6 || !strings.EqualFold(f[5], "LISTEN") {
			continue
		}
		addr, port, ok := splitHostPort(f[3])
		if !ok || port < minInterestingPort || port > maxInterestingPort {
			continue
		}
		row := portRow{addr: addr, port: port}
		if len(f) >= 7 {
			if pid, name, ok := strings.Cut(f[6], "/"); ok {
				if n, err := strconv.Atoi(pid); err == nil {
					row.pid = n
				}
				row.name = name
			}
		}
		rows = append(rows, row)
	}
	return rows
}
