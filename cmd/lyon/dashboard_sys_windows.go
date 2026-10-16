//go:build windows

package main

// Local machine probe for the dashboard — pure syscalls, no cgo, no
// external deps beyond the x/sys/windows module already in go.mod.
// Every field that cannot be measured returns its "unavailable" value
// (-1 percent / false OK) and the renderer shows an ellipsis.

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	dllKernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes   = dllKernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStat = dllKernel32.NewProc("GlobalMemoryStatusEx")
	dllIphlpapi          = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIfTable       = dllIphlpapi.NewProc("GetIfTable")
	dllNtdll             = windows.NewLazySystemDLL("ntdll.dll")
	procRtlGetVersion    = dllNtdll.NewProc("RtlGetVersion")
)

type sysFiletime struct{ Low, High uint32 }

func (f sysFiletime) u64() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

var (
	sysPrevMu  sync.Mutex
	sysPrevCPU struct {
		ok         bool
		at         time.Time
		idle, k, u uint64
	}
	sysPrevNet struct {
		ok      bool
		at      time.Time
		in, out uint32
	}
)

// cpuPct samples GetSystemTimes and diffs against the previous sample.
// First call (or a sample pair less than 100ms apart) reports -1 — the
// renderer prints an ellipsis until real deltas exist.
func cpuPct() float64 {
	var idle, kernel, user sysFiletime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return -1
	}
	now := time.Now()
	i, k, u := idle.u64(), kernel.u64(), user.u64()

	sysPrevMu.Lock()
	defer sysPrevMu.Unlock()
	pct := -1.0
	if sysPrevCPU.ok && now.Sub(sysPrevCPU.at) >= 100*time.Millisecond {
		di, dk, du := i-sysPrevCPU.idle, k-sysPrevCPU.k, u-sysPrevCPU.u
		// dwKernelTime already includes idle time.
		dtotal := dk + du
		if dtotal > 0 {
			pct = 100 * (1 - float64(di)/float64(dtotal))
			if pct < 0 {
				pct = 0
			} else if pct > 100 {
				pct = 100
			}
		}
	}
	sysPrevCPU.ok = true
	sysPrevCPU.at = now
	sysPrevCPU.idle, sysPrevCPU.k, sysPrevCPU.u = i, k, u
	return pct
}

type sysMemStatus struct {
	Length          uint32
	MemoryLoad      uint32
	TotalPhys       uint64
	AvailPhys       uint64
	TotalPageFile   uint64
	AvailPageFile   uint64
	TotalVirtual    uint64
	AvailVirtual    uint64
	AvailExtVirtual uint64
}

func memorySnap() (pct float64, used, total uint64, ok bool) {
	var ms sysMemStatus
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMemoryStat.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return -1, 0, 0, false
	}
	return float64(ms.MemoryLoad), ms.TotalPhys - ms.AvailPhys, ms.TotalPhys, true
}

// dashDiskRoot maps any workspace path onto its volume root for the free
// space probe; an empty workspace falls back to the current directory's
// volume, and to C:\ if even that has none.
func dashDiskRoot(workspace string) string {
	p := workspace
	if p == "" {
		if cwd, err := filepath.Abs("."); err == nil {
			p = cwd
		} else {
			p = `C:\`
		}
	}
	if vol := filepath.VolumeName(p); vol != "" {
		return vol + `\`
	}
	return `C:\`
}

func diskSnap(root string) (free, total uint64, ok bool) {
	dir, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, false
	}
	var freeAvail, totalBytes, totalFree uint64
	err = windows.GetDiskFreeSpaceEx(dir, &freeAvail, &totalBytes, &totalFree)
	if err != nil {
		return 0, 0, false
	}
	return freeAvail, totalBytes, true
}

// MIB_IFROW layout (verified by struct math): 512-byte name + 8-byte
// physAddr + dwords; dwType sits at 516, dwInOctets at 552, dwOutOctets
// at 576, row stride is 860 bytes inside the MIB_IFROW table.
const (
	sysRowSize    = 860
	sysOffType    = 516
	sysOffIn      = 552
	sysOffOut     = 576
	sysIfLoopback = 24 // IF_TYPE_SOFTWARE_LOOPBACK
)

// netSample reads every interface's octet counters (two-call size
// pattern) and diffs against the previous sample for B/s rates.
func netSample() (up, down float64, ok bool) {
	var size uint32
	procGetIfTable.Call(0, uintptr(unsafe.Pointer(&size)), 0) // ERROR_INSUFFICIENT_BUFFER
	if size < 4+sysRowSize {
		return 0, 0, false
	}
	buf := make([]byte, size)
	r, _, _ := procGetIfTable.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if r != 0 {
		return 0, 0, false
	}
	n := binary.LittleEndian.Uint32(buf[0:4])
	var inSum, outSum uint64
	for i := uint32(0); 4+(i+1)*sysRowSize <= uint32(len(buf)) && i < n; i++ {
		base := 4 + i*sysRowSize
		typ := binary.LittleEndian.Uint32(buf[base+sysOffType:])
		if typ == sysIfLoopback {
			continue
		}
		inSum += uint64(binary.LittleEndian.Uint32(buf[base+sysOffIn:]))
		outSum += uint64(binary.LittleEndian.Uint32(buf[base+sysOffOut:]))
	}
	now := time.Now()
	sysPrevMu.Lock()
	defer sysPrevMu.Unlock()
	if sysPrevNet.ok && now.Sub(sysPrevNet.at) >= 100*time.Millisecond {
		// uint32 subtraction wraps safely across counter rollover.
		dIn := uint32(uint32(inSum) - sysPrevNet.in)
		dOut := uint32(uint32(outSum) - sysPrevNet.out)
		dt := now.Sub(sysPrevNet.at).Seconds()
		up = float64(dOut) / dt
		down = float64(dIn) / dt
		ok = true
	}
	sysPrevNet.ok = true
	sysPrevNet.at = now
	sysPrevNet.in = uint32(inSum)
	sysPrevNet.out = uint32(outSum)
	return up, down, ok
}

type sysOSVersionInfo struct {
	Size     uint32
	Major    uint32
	Minor    uint32
	Build    uint32
	Platform uint32
	CSD      [128]uint16
}

func osName() string {
	var v sysOSVersionInfo
	v.Size = uint32(unsafe.Sizeof(v))
	r, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&v)))
	if r == 0 && v.Major > 0 {
		return fmt.Sprintf("windows %d.%d.%d", v.Major, v.Minor, v.Build)
	}
	// RtlGetVersion never lies the way GetVersionEx does; this is a
	// belt-and-braces fallback so the row always has a value.
	return "windows (ver " + runtime.GOARCH + ")"
}

func sysProbe(workspace string) sysSnap {
	pct, used, total, memOK := memorySnap()
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
