//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// requirePidfd skips when the kernel or sandbox refuses pidfd_open.
func requirePidfd(t *testing.T) {
	t.Helper()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd_open unavailable: %v", err)
	}
	_ = unix.Close(fd)
}

// withPidfdOpen replaces pidfdOpen for the duration of the test.
func withPidfdOpen(t *testing.T, f func(int) (int, error)) {
	t.Helper()
	orig := pidfdOpen
	pidfdOpen = f
	t.Cleanup(func() { pidfdOpen = orig })
}

// requireSignalled fails unless err is an exit by signal sig.
func requireSignalled(t *testing.T, err error, sig syscall.Signal) {
	t.Helper()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Wait = %v, want exit by %v", err, sig)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != sig {
		t.Fatalf("exit status %v, want %v", ee.ProcessState, sig)
	}
}

func TestHandleUsesPidfd(t *testing.T) {
	requirePidfd(t)
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(id, boot)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.fd < 0 {
		t.Fatal("handle holds no pidfd")
	}
	if !h.Alive() || h.Identity() != id {
		t.Fatalf("handle: alive %v, identity %+v", h.Alive(), h.Identity())
	}
	if err := h.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	requireSignalled(t, cmd.Wait(), syscall.SIGTERM)
}

func TestSignalViaPidfdLiveChild(t *testing.T) {
	requirePidfd(t)
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := Signal(id, boot, syscall.SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	requireSignalled(t, cmd.Wait(), syscall.SIGKILL)
	if err := Signal(id, boot, syscall.SIGKILL); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("Signal after reap = %v, want ErrNoProcess", err)
	}
}

// A handle held across the target's exit and reap refuses to signal: the
// pidfd still names the dead instance, so pidfd_send_signal gives ESRCH even
// if the pid has meanwhile been reused.
func TestHandleRefusesAfterReap(t *testing.T) {
	requirePidfd(t)
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(id, boot)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state, _, _, err := readStat(id.PID); err == nil && state == 'Z' {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Unreaped, the zombie pins the pid; the signal is accepted and ignored.
	if err := h.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal to zombie = %v, want nil", err)
	}
	_ = cmd.Wait()
	if err := h.Signal(syscall.SIGKILL); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("Signal after reap = %v, want ErrNoProcess", err)
	}
	if h.Alive() {
		t.Fatal("reaped process reported alive")
	}
}

// The process dies after the pidfd is opened but before identity is
// verified: the verification fails and nothing is signalled.
func TestOpenVerifiesAfterPin(t *testing.T) {
	requirePidfd(t)
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	withPidfdOpen(t, func(pid int) (int, error) {
		fd, err := unix.PidfdOpen(pid, 0)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fd, err
	})
	if h, err := Open(id, boot); !errors.Is(err, ErrNoProcess) {
		if h != nil {
			h.Close()
		}
		t.Fatalf("Open = %v, want ErrNoProcess", err)
	}
}

func TestPidfdFallback(t *testing.T) {
	for _, e := range []error{unix.ENOSYS, unix.EPERM} {
		t.Run(e.Error(), func(t *testing.T) {
			calls := 0
			withPidfdOpen(t, func(int) (int, error) { calls++; return -1, e })
			boot := mustBoot(t)
			cmd := startInGroup(t, os.Environ())
			id, err := Of(cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			h, err := Open(id, boot)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer h.Close()
			if h.fd != -1 || calls != 1 {
				t.Fatalf("fd %d after %d calls, want -1 after 1", h.fd, calls)
			}
			bad := Identity{PID: id.PID, StartTime: id.StartTime + 1}
			if err := Signal(bad, boot, syscall.SIGKILL); !errors.Is(err, ErrNoProcess) {
				t.Fatalf("Signal(bad) = %v, want ErrNoProcess", err)
			}
			time.Sleep(50 * time.Millisecond)
			if !Alive(id, boot) {
				t.Fatal("target signalled despite identity mismatch")
			}
			if err := h.Signal(syscall.SIGKILL); err != nil {
				t.Fatalf("fallback Signal: %v", err)
			}
			_ = cmd.Wait()
			if err := h.Signal(syscall.SIGKILL); !errors.Is(err, ErrNoProcess) {
				t.Fatalf("fallback Signal after reap = %v, want ErrNoProcess", err)
			}
		})
	}
}

func TestOpenESRCH(t *testing.T) {
	withPidfdOpen(t, func(int) (int, error) { return -1, unix.ESRCH })
	id, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(id, mustBoot(t)); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("Open = %v, want ErrNoProcess", err)
	}
}
