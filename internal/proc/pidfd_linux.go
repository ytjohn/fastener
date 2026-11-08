//go:build linux

package proc

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdOpen is unix.PidfdOpen; tests replace it to simulate old kernels.
var pidfdOpen = func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

// openPin opens a pidfd for pid. It returns -1 and no error when pidfds are
// unavailable (ENOSYS before Linux 5.3, EPERM under seccomp, or any other
// refusal), so the caller falls back to check-then-kill; that fallback is
// the pre-pidfd behaviour and nothing is reported. ESRCH is ErrNoProcess.
func openPin(pid int) (int, error) {
	fd, err := pidfdOpen(pid)
	if err == nil {
		return fd, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return -1, ErrNoProcess
	}
	return -1, nil
}

func signalPin(fd int, sig syscall.Signal) error {
	if err := unix.PidfdSendSignal(fd, sig, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return ErrNoProcess
		}
		return err
	}
	return nil
}

func closePin(fd int) { _ = unix.Close(fd) }
