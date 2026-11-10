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

// agentPTYSysProcAttr starts an interactive agent in a new session whose
// controlling terminal is the pty on fd 0. As session leader it also leads
// its own process group (pgid = pid), as a headless agent does; Setpgid
// must not be added, since setpgid fails for a session leader.
func agentPTYSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Pdeathsig: syscall.SIGKILL}
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

// exitedNoReap reports, without blocking or reaping, whether pid has already
// exited (it is then a zombie). It sees an exit before the waiter goroutine
// has published it on exitPending.
func exitedNoReap(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if err == unix.EINTR {
			continue
		}
		return err == nil && info.Signo != 0
	}
}
