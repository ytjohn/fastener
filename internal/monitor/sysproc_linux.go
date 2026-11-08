//go:build linux

package monitor

import "syscall"

// The agent leads its own process group; Pdeathsig kills it if the monitor's
// spawning thread dies (the thread stays locked for the session's lifetime).
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
