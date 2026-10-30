//go:build windows

package tools

import (
	"os/exec"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setupBg runs before Start: nothing needed on Windows — group/job
// membership is assigned explicitly once the pid exists.
func setupBg(cmd *exec.Cmd) {}

// attachBg runs right after Start: give the process its own job object
// with KILL_ON_JOB_CLOSE, so every handle drop — waiter release after a
// natural exit, registry sweep at engine close, or lyond itself being
// hard-killed (handles close with the process) — takes the whole tree
// down. Unnamed job: no global-namespace collisions across lyond
// instances. Failure degrades to pid-based tree kills only.
func attachBg(e *bgEntry) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(e.pid))
	if err != nil {
		windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return err
	}
	e.job = uintptr(job)
	return nil
}

// killBgTree terminates the whole tree from its job. The pid fallback
// covers entries whose attach failed (job == 0).
func killBgTree(e *bgEntry) {
	if e.job != 0 {
		windows.TerminateJobObject(windows.Handle(e.job), 1)
		return
	}
	if e.pid > 0 {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(e.pid)).Run()
	}
}

// releaseBg drops the job handle; with KILL_ON_JOB_CLOSE set, any
// stragglers the main process left behind go with it — a finished
// background task never leaves a tree on the box. Callers hold bgMu so
// this serializes with killBgTree (never touch a handle mid-close).
func releaseBg(e *bgEntry) {
	if e.job != 0 {
		windows.CloseHandle(windows.Handle(e.job))
		e.job = 0
	}
}
