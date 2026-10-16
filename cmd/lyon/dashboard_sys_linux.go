//go:build linux

package main

// Local machine probe for the dashboard on Linux: /proc and statfs only,
// no cgo, no deps. Unavailable values (-1 / false) render as ellipsis.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	sysPrevMu  sync.Mutex
	sysPrevCPU struct {
		ok          bool
		at          time.Time
		idle, total uint64
	}
	sysPrevNet struct {
		ok      bool
		at      time.Time
		in, out uint64
	}
)

// cpuPct diffs /proc/stat against the previous sample. First call (or a
// pair under 100ms apart) reports -1.
func cpuPct() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 5 || fields[0] != "cpu" {
		return -1
	}
	// Fields: cpu user nice system idle iowait irq softirq steal ...
	var vals []uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, v)
	}
	if len(vals) < 4 {
		return -1
	}
	idleT := vals[3]
	var total uint64
	for _, v := range vals {
		total += v
	}
	if len(vals) >= 5 {
		idleT += vals[4] // iowait counts as idle
	}
	now := time.Now()

	sysPrevMu.Lock()
	defer sysPrevMu.Unlock()
	pct := -1.0
	if sysPrevCPU.ok && now.Sub(sysPrevCPU.at) >= 100*time.Millisecond {
		dIdle := idleT - sysPrevCPU.idle
		dTotal := total - sysPrevCPU.total
		if dTotal > 0 {
			pct = 100 * (1 - float64(dIdle)/float64(dTotal))
			if pct < 0 {
				pct = 0
			} else if pct > 100 {
				pct = 100
			}
		}
	}
	sysPrevCPU.ok = true
	sysPrevCPU.at = now
	sysPrevCPU.idle, sysPrevCPU.total = idleT, total
	return pct
}

// memInfo parses MemTotal / MemAvailable (kB) out of /proc/meminfo.
func memInfo() (pct float64, used, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1, 0, 0, false
	}
	var totalKB, availKB uint64
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = parseKB(line)
		}
	}
	if totalKB == 0 {
		return -1, 0, 0, false
	}
	usedKB := totalKB - availKB
	return float64(usedKB) / float64(totalKB) * 100, usedKB * 1024, totalKB * 1024, true
}

func parseKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// dashDiskRoot maps a workspace onto the directory statfs should measure
// (the directory itself must exist; fall back to cwd then "/").
func dashDiskRoot(workspace string) string {
	if workspace != "" {
		if st, err := os.Stat(workspace); err == nil && st.IsDir() {
			return workspace
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "/"
}

func diskSnap(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, true
}

// netSample sums non-loopback interfaces from /proc/net/dev (column 0 =
// rx bytes after the colon; column 8 = tx bytes) and diffs against the
// previous sample for B/s.
func netSample() (up, down float64, ok bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var inSum, outSum uint64
	sc := bufio.NewScanner(f)
	skipped := 0
	for sc.Scan() {
		line := sc.Text()
		if skipped < 2 { // two header lines
			skipped++
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "lo" {
			continue
		}
		cols := strings.Fields(line[idx+1:])
		if len(cols) < 9 {
			continue
		}
		v, err := strconv.ParseUint(cols[0], 10, 64)
		if err == nil {
			inSum += v
		}
		v, err = strconv.ParseUint(cols[8], 10, 64)
		if err == nil {
			outSum += v
		}
	}
	now := time.Now()
	sysPrevMu.Lock()
	defer sysPrevMu.Unlock()
	if sysPrevNet.ok && now.Sub(sysPrevNet.at) >= 100*time.Millisecond {
		dt := now.Sub(sysPrevNet.at).Seconds()
		up = float64(outSum-sysPrevNet.out) / dt
		down = float64(inSum-sysPrevNet.in) / dt
		ok = true
	}
	sysPrevNet.ok = true
	sysPrevNet.at = now
	sysPrevNet.in, sysPrevNet.out = inSum, outSum
	return up, down, ok
}

func osName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, hit := strings.CutPrefix(line, "PRETTY_NAME="); hit {
				return strings.Trim(v, `"'`)
			}
		}
	}
	return "linux"
}

// sysEnsureBaseline primes the CPU and net rate samplers when this
// process has no previous sample: one throwaway read, one sampling
// window, then the caller's real read lands inside cpuPct/netSample —
// the first sysProbe after start reports actual rates instead of the
// -1/false warmup placeholder.
func sysEnsureBaseline() {
	sysPrevMu.Lock()
	need := !sysPrevCPU.ok || !sysPrevNet.ok
	sysPrevMu.Unlock()
	if !need {
		return
	}
	_ = cpuPct()
	_, _, _ = netSample()
	time.Sleep(120 * time.Millisecond)
}

// sysResetBaseline drops the primed samples (test hook: re-prove the
// first-sample warmup).
func sysResetBaseline() {
	sysPrevMu.Lock()
	defer sysPrevMu.Unlock()
	sysPrevCPU.ok = false
	sysPrevNet.ok = false
}

func sysProbe(workspace string) sysSnap {
	sysEnsureBaseline()
	pct, used, total, memOK := memInfo()
	free, dtot, diskOK := diskSnap(dashDiskRoot(workspace))
	up, down, netOK := netSample()
	s := sysSnap{
		CPUPct:     cpuPct(),
		MemPct:     -1,
		NetUpBps:   up,
		NetDownBps: down,
		NetOK:      netOK,
		OS:         osName(),
	}
	if memOK {
		s.MemPct = pct
		s.MemUsed = used
		s.MemTotal = total
	}
	if diskOK {
		s.DiskFree = free
		s.DiskTotal = dtot
	}
	return s
}
