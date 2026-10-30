//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setupBg runs before Start: the child leads its own process group, so
// one signal takes the whole tree and teardown can never hit unrelated
// processes that happen to share our group.
func setupBg(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// attachBg: the group was set before Start — nothing to assign.
func attachBg(e *bgEntry) error { return nil }

// killBgTree signals the process group. Entries sit in the registry only
// while the leader is alive (Wait has not returned), so the pgid is
// still ours. ESRCH just means it won the race to exit.
func killBgTree(e *bgEntry) {
	if e.pid > 0 {
		_ = syscall.Kill(-e.pid, syscall.SIGKILL)
	}
}

// releaseBg: POSIX has no kill-on-close handle — stragglers that dodged
// the group signal are the command's own doing (`&`), and reaping the
// leader already ended the task proper.
func releaseBg(e *bgEntry) {}
