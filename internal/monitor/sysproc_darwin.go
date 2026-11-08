//go:build darwin

package monitor

import "syscall"

// macOS has no parent-death signal; orphans are found by marker (spec §4).
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// Known darwin gap: x/sys/unix has no Waitid, so the exited agent cannot be
// held as a zombie while the group is signalled. The spawn goroutine reaps
// immediately and the pgid is not pinned.
const pinsZombie = false

func waitExitNoReap(pid int) {}

func exitedNoReap(pid int) bool { return false }
