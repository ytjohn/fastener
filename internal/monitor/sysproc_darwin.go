//go:build darwin

package monitor

import "syscall"

// macOS has no parent-death signal; orphans are found by marker (spec §4).
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
