//go:build darwin

package main

// Local machine probe for the dashboard on macOS. CPU sampling and
// per-interface counters would need Mach host_statistics / sysctl
// NET_RT_IFLIST2 — deliberately out of scope (stdlib-only); those cells
// report -1/false and render as ellipsis. Total memory, disk and the OS
// version all come from plain syscalls.

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
)

// sysProbe gathers what macOS exposes without cgo. CPU percent is never
// measured here (-1); used-memory percentage is unknown (only total is
// readable via hw.memsize), so MemPct stays -1 while MemTotal is filled.
func sysProbe(workspace string) sysSnap {
	s := sysSnap{
		CPUPct:   -1,
		MemPct:   -1,
		NetOK:    false,
		OS:       osName(),
		DiskFree: 0,
	}
	if v, err := syscall.Sysctl("hw.memsize"); err == nil {
		if total, err := strconv.ParseUint(v, 10, 64); err == nil {
			s.MemTotal = total
		}
	}
	free, total, ok := diskSnap(dashDiskRoot(workspace))
	if ok {
		s.DiskFree = free
		s.DiskTotal = total
	}
	return s
}

// dashDiskRoot keeps the same shape as the other platforms: measure the
// workspace volume when possible, else the current directory.
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

// darwin's Statfs_t.Bsize is a uint32 (unlike linux's int64) — the
// cross-build check pins this.
func diskSnap(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, true
}

func osName() string {
	if v, err := syscall.Sysctl("kern.osproductversion"); err == nil && v != "" {
		return "macOS " + v
	}
	return runtime.GOOS
}

// No rate samplers to prime on darwin (CPU/net report unavailable by
// design) — kept so shared test hooks compile on this platform.
func sysEnsureBaseline() {}

func sysResetBaseline() {}
