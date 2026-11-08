//go:build darwin

package monitor

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ytjohn/fastener/internal/proc"
)

// TestKeventOnZombie records what registering NOTE_EXIT on an existing zombie
// does: ESRCH (the race waitExitNoReap falls back from), or an immediate
// NOTE_EXIT. Either way waitExitNoReap must return; this test pins down which
// path TestWaitExitNoReapAfterExit exercised.
func TestKeventOnZombie(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	pid := cmd.Process.Pid
	waitFor(t, "child zombie", func() bool { z, _ := proc.IsZombie(pid); return z })
	kq, err := newKqueue()
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(kq)
	if fl, err := unix.FcntlInt(uintptr(kq), unix.F_GETFD, 0); err != nil || fl&unix.FD_CLOEXEC == 0 {
		t.Fatalf("kqueue fd flags %#x, %v: want FD_CLOEXEC", fl, err)
	}
	var ev [1]unix.Kevent_t
	unix.SetKevent(&ev[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev[0].Fflags = unix.NOTE_EXIT
	_, err = unix.Kevent(kq, ev[:], nil, nil)
	t.Logf("kevent(EV_ADD NOTE_EXIT) on a zombie: err=%v", err)
	if err == nil {
		n, err := unix.Kevent(kq, nil, ev[:], &unix.Timespec{})
		t.Logf("zero-timeout poll: n=%d err=%v fflags=%#x", n, err, ev[0].Fflags)
	} else if err != unix.ESRCH {
		t.Fatalf("unexpected kevent error %v", err)
	}
}
