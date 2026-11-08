//go:build linux

package monitor

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// The agent leads its own process group; Pdeathsig kills it if the monitor's
// spawning thread dies (the thread stays locked for the session's lifetime).
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// pinsZombie reports that waitExitNoReap is supported: the dead agent stays a
// zombie until cmd.Wait, so its pid and pgid cannot be reused while we signal.
const pinsZombie = true

// waitExitNoReap blocks until pid has exited without reaping it (WNOWAIT).
func waitExitNoReap(pid int) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return
		}
	}
}
