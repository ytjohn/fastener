//go:build darwin

package monitor

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ytjohn/fastener/internal/proc"
)

// macOS has no parent-death signal, so an agent can outlive a killed monitor;
// it is then found by marker and handled as an orphan (design §4).
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// agentPTYSysProcAttr starts an interactive agent in a new session whose
// controlling terminal is the pty on fd 0. As session leader it also leads
// its own process group (pgid = pid), as a headless agent does; Setpgid
// must not be added, since setpgid fails for a session leader.
func agentPTYSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}

// pinsZombie reports that waitExitNoReap is supported: the dead agent stays a
// zombie until cmd.Wait, so its pid and pgid cannot be reused while we signal.
// x/sys/unix has no Waitid on darwin; the exit is observed with kqueue
// EVFILT_PROC/NOTE_EXIT instead, which does not reap.
const pinsZombie = true

// waitExitNoReap blocks until pid has exited, without reaping it. It is called
// by the spawn goroutine right after cmd.Start. If the agent is already
// exiting when the knote is registered, kevent fails with ESRCH; it then waits
// for the zombie instead.
func waitExitNoReap(pid int) {
	kq, err := newKqueue()
	if err != nil {
		waitZombie(pid)
		return
	}
	defer unix.Close(kq)
	var ev [1]unix.Kevent_t
	unix.SetKevent(&ev[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev[0].Fflags = unix.NOTE_EXIT
	for {
		_, err = unix.Kevent(kq, ev[:], nil, nil) // re-adding is idempotent
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		waitZombie(pid) // ESRCH: exited (or exiting) before registration
		return
	}
	for {
		n, err := unix.Kevent(kq, nil, ev[:], nil)
		switch {
		case err == unix.EINTR || (err == nil && n == 0):
			continue
		case err != nil:
			waitZombie(pid)
			return
		case ev[0].Fflags&unix.NOTE_EXIT != 0:
			return
		}
	}
}

// newKqueue returns a close-on-exec kqueue. kqueue(2) sets no FD_CLOEXEC (and
// x/sys does not add it); a kqueue is not inherited across fork on macOS
// anyway, but the flag is set under ForkLock as Go does for its own fds.
func newKqueue() (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	kq, err := unix.Kqueue()
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(kq)
	return kq, nil
}

// waitZombie polls until pid is a zombie (or gone, which cannot happen before
// cmd.Wait but must not hang). It is the fallback when kqueue is unusable or
// the exit raced the registration.
func waitZombie(pid int) {
	for d := time.Millisecond; ; d = min(2*d, 50*time.Millisecond) {
		z, err := proc.IsZombie(pid)
		if z || errors.Is(err, proc.ErrNoProcess) {
			return
		}
		time.Sleep(d)
	}
}

// exitedNoReap reports, without blocking or reaping, whether pid has already
// exited (it is then a zombie, p_stat SZOMB). It sees an exit before the
// waiter goroutine has published it on exitPending.
func exitedNoReap(pid int) bool {
	z, err := proc.IsZombie(pid)
	return err == nil && z
}
