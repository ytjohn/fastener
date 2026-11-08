package monitor

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
)

// These tests exercise the pinned flow (pinsZombie): the dead agent stays an
// unreaped zombie while its group is signalled. They run on Linux (waitid
// WNOWAIT) and macOS (kqueue NOTE_EXIT).

// dead reports whether pid is a zombie or gone.
func dead(pid int) bool {
	z, err := proc.IsZombie(pid)
	return z || errors.Is(err, proc.ErrNoProcess)
}

func TestPinsZombie(t *testing.T) {
	if !pinsZombie {
		t.Fatal("pinsZombie is false on this platform")
	}
}

// TestWaitExitNoReapLeavesZombie registers the wait while the child runs:
// waitExitNoReap returns once it exits, the child is still an unreaped zombie
// (pid pinned), and cmd.Wait then reaps it with its real exit status.
func TestWaitExitNoReapLeavesZombie(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "read x; exit 7")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	returned := make(chan struct{})
	go func() { waitExitNoReap(pid); close(returned) }()
	select {
	case <-returned:
		t.Fatal("waitExitNoReap returned while the child was running")
	case <-time.After(200 * time.Millisecond):
	}
	if exitedNoReap(pid) {
		t.Fatal("exitedNoReap reported a running child as exited")
	}
	io.WriteString(stdin, "go\n")
	stdin.Close()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("waitExitNoReap did not return after the child exited")
	}
	assertPinnedThenReap(t, cmd, 7)
}

// TestWaitExitNoReapAfterExit covers the registration race: the child is
// already a zombie when waitExitNoReap is called (on macOS kevent then fails
// with ESRCH and the zombie check takes over).
func TestWaitExitNoReapAfterExit(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	waitFor(t, "child zombie", func() bool { z, _ := proc.IsZombie(pid); return z })
	returned := make(chan struct{})
	go func() { waitExitNoReap(pid); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("waitExitNoReap did not return for an already exited child")
	}
	assertPinnedThenReap(t, cmd, 7)
}

func assertPinnedThenReap(t *testing.T, cmd *exec.Cmd, code int) {
	t.Helper()
	pid := cmd.Process.Pid
	waitFor(t, "exitedNoReap", func() bool { return exitedNoReap(pid) })
	if z, err := proc.IsZombie(pid); !z || err != nil {
		t.Fatalf("exited child not pinned as a zombie: zombie=%v err=%v", z, err)
	}
	if err := cmd.Wait(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != code {
		t.Fatalf("cmd.Wait: %v, state %v; want exit %d", err, cmd.ProcessState, code)
	}
	if _, err := proc.IsZombie(pid); !errors.Is(err, proc.ErrNoProcess) {
		t.Fatalf("reaped child still visible: %v", err)
	}
}

func TestStopKillsGrandchild(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile, "FAKE_SLEEP=30s"}, 0)
	sigs := make(chan os.Signal, 4)
	st := runWith(t, d, sigs, func() {
		waitFor(t, "grandchild pidfile", func() bool { b, err := os.ReadFile(pidfile); return err == nil && len(b) > 0 })
		requestStop(t, d)
		sigs <- syscall.SIGTERM
	})
	if st.EndedBy != session.EndedByStop {
		t.Fatalf("ended_by=%s", st.EndedBy)
	}
	b, _ := os.ReadFile(pidfile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild pidfile %q: %v", b, err)
	}
	waitFor(t, "grandchild dead", func() bool { return dead(pid) })
}

// TestExitBeatsSignal makes the agent exit (and sit pinned as a zombie) and a
// signal ready at the same moment; the real exit must win.
func TestExitBeatsSignal(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_EXIT=3"}, 0)
		sigs := make(chan os.Signal, 4)
		m, err := load(d, 1, log.New(os.Stderr, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		m.beforeWait = func(pid int) {
			// On Linux /proc shows Z as soon as the thread-group leader dies, even
			// while other threads of the agent still run; exitedNoReap (waitid
			// readiness, or SZOMB on macOS) means fully exited.
			waitFor(t, "agent exited", func() bool { return exitedNoReap(pid) })
			sigs <- syscall.SIGTERM
		}
		done := make(chan int, 1)
		go func() { done <- m.run(sigs) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("run did not finish")
		}
		var st session.Status
		if err := session.ReadJSON(d.StatusPath(), &st); err != nil {
			t.Fatal(err)
		}
		if st.EndedBy != session.EndedByExit || st.ExitCode == nil || *st.ExitCode != 3 || st.Signal != "" {
			t.Fatalf("iter %d: ended_by=%s code=%v signal=%s", i, st.EndedBy, st.ExitCode, st.Signal)
		}
	}
}
