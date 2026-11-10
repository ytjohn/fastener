package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/ytjohn/fastener/internal/attach"
	"github.com/ytjohn/fastener/internal/session"
)

// attachProc is `fastener attach` running on a pty the test owns, as a
// user's terminal would be: the test types into the master and reads what
// the terminal shows (stdout and stderr both).
type attachProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	slave  *os.File
	before *unix.Termios

	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
	code int
}

func startAttach(t *testing.T, h *harness, args ...string) *attachProc {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	before, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fastenerBin, append([]string{"attach"}, args...)...)
	cmd.Env = append(append([]string{}, h.extraEnv...),
		"FASTENER_STATE_DIR="+h.state, "FASTENER_CONFIG_DIR="+h.config,
		"PATH="+os.Getenv("PATH"), "HOME="+os.Getenv("HOME"), "TMPDIR="+h.work)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Its own session with the pty as controlling terminal, so resizing the
	// pty sends it SIGWINCH.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &attachProc{t: t, cmd: cmd, master: master, slave: slave, before: before, done: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			a.mu.Lock()
			a.out.Write(buf[:n])
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		a.code = cmd.ProcessState.ExitCode()
		if ee, ok := err.(*exec.ExitError); ok && ee.ProcessState.ExitCode() == -1 {
			a.code = -1 // killed by a signal it did not handle
		}
		close(a.done)
	}()
	t.Cleanup(func() {
		select {
		case <-a.done:
		default:
			_ = cmd.Process.Kill()
			<-a.done
		}
		slave.Close()
		master.Close()
	})
	return a
}

func (a *attachProc) output() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out.String()
}

func (a *attachProc) waitOutput(want string) {
	a.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(a.output(), want) {
			return
		}
	}
	a.t.Fatalf("terminal never showed %q:\n%q", want, a.output())
}

func (a *attachProc) typeIn(s string) {
	a.t.Helper()
	if _, err := a.master.Write([]byte(s)); err != nil {
		a.t.Fatal(err)
	}
}

// wait waits for attach to exit and returns its exit code.
func (a *attachProc) wait() int {
	a.t.Helper()
	select {
	case <-a.done:
	case <-time.After(15 * time.Second):
		a.t.Fatalf("attach did not exit; terminal:\n%q", a.output())
	}
	return a.code
}

// termiosRestored checks the terminal is in the mode it had before attach.
func (a *attachProc) termiosRestored() {
	a.t.Helper()
	after, err := unix.IoctlGetTermios(int(a.slave.Fd()), ioctlGetTermios)
	if err != nil {
		a.t.Fatal(err)
	}
	b := a.before
	if after.Iflag != b.Iflag || after.Oflag != b.Oflag || after.Cflag != b.Cflag || after.Lflag != b.Lflag || after.Cc != b.Cc {
		a.t.Fatalf("terminal not restored:\nbefore %+v\nafter  %+v", *b, *after)
	}
}

// rawDuring checks attach put the terminal in raw mode (no echo, no
// canonical input) while attached.
func (a *attachProc) rawDuring() {
	a.t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		tio, err := unix.IoctlGetTermios(int(a.slave.Fd()), ioctlGetTermios)
		if err == nil && tio.Lflag&(unix.ECHO|unix.ICANON) == 0 {
			return
		}
	}
	a.t.Fatal("attach never put the terminal in raw mode")
}

func runningPTY(t *testing.T, h *harness) {
	t.Helper()
	d := startPTYSession(t, h, "setsid")
	_ = d
	poll(t, "running", func() bool { return inspectPTY(t, h).State == session.StateRunning })
}

// TestAttachEchoResizeDetach: keys reach the agent and its output reaches
// the terminal; resizing the terminal reaches the agent; the detach key
// leaves the agent running and the terminal as it was.
func TestAttachEchoResizeDetach(t *testing.T) {
	h := newHarness(t)
	runningPTY(t, h)
	a := startAttach(t, h, "pty1")
	a.waitOutput("winch=30x100") // attaching applied the terminal's size
	a.rawDuring()
	a.typeIn("hello\r")
	a.waitOutput("echo:hello")
	if err := pty.Setsize(a.master, &pty.Winsize{Rows: 41, Cols: 132}); err != nil {
		t.Fatal(err)
	}
	a.waitOutput("winch=41x132")
	a.typeIn("\x1d") // ctrl-]
	if code := a.wait(); code != 0 {
		t.Fatalf("detach exited %d; terminal:\n%q", code, a.output())
	}
	a.termiosRestored()
	if !strings.Contains(a.output(), "[fastener: detached from pty1]") {
		t.Fatalf("no detach line:\n%q", a.output())
	}
	if v := inspectPTY(t, h); v.State != session.StateRunning {
		t.Fatalf("detaching ended the session: %+v", v)
	}
	// Attach again and leave via a configured key.
	if err := h.writeConfig(`detach_key = "ctrl-b"` + "\n"); err != nil {
		t.Fatal(err)
	}
	b := startAttach(t, h, "pty1", "--quiet")
	b.waitOutput("winch=30x100")
	b.typeIn("\x1d") // no longer the detach key: sent to the agent
	b.typeIn("again\r")
	b.waitOutput("echo:\x1dagain")
	b.typeIn("\x02") // ctrl-b
	if code := b.wait(); code != 0 {
		t.Fatalf("ctrl-b detach exited %d:\n%q", code, b.output())
	}
	b.termiosRestored()
	if strings.Contains(b.output(), "attached to") {
		t.Fatalf("--quiet printed the hint:\n%q", b.output())
	}
}

// TestAttachSessionExit: the session ending while attached restores the
// terminal and exits with the agent's code.
func TestAttachSessionExit(t *testing.T) {
	h := newHarness(t)
	runningPTY(t, h)
	a := startAttach(t, h, "pty1", "--detach-key", "none")
	a.waitOutput("winch=30x100")
	if !strings.Contains(a.output(), "detach key is disabled") {
		t.Fatalf("hint:\n%q", a.output())
	}
	a.typeIn("\x1d\r") // no detach key: just a byte, on a line of its own
	a.waitOutput("echo:\x1d")
	a.typeIn("exit 5\r")
	if code := a.wait(); code != 5 {
		t.Fatalf("exit code %d, want the agent's 5; terminal:\n%q", code, a.output())
	}
	a.termiosRestored()
	if !strings.Contains(a.output(), "[fastener: session pty1 exited 5]") {
		t.Fatalf("no exit line:\n%q", a.output())
	}
}

// TestAttachSignalRestores: SIGTERM to attach (not the agent) restores the
// terminal, exits 128+15, and leaves the session running.
func TestAttachSignalRestores(t *testing.T) {
	h := newHarness(t)
	runningPTY(t, h)
	a := startAttach(t, h, "pty1")
	a.waitOutput("winch=30x100")
	a.rawDuring()
	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := a.wait(); code != 128+15 {
		t.Fatalf("exit code %d, want 143; terminal:\n%q", code, a.output())
	}
	a.termiosRestored()
	if v := inspectPTY(t, h); v.State != session.StateRunning {
		t.Fatalf("SIGTERM to attach ended the session: %+v", v)
	}
	poll(t, "client gone", func() bool {
		v := inspectPTY(t, h)
		return v.Status.AttachedClients != nil && *v.Status.AttachedClients == 0
	})
}

// TestAttachBusyRefused: a monitor with every client slot taken refuses
// attach, which exits 1 without touching the terminal.
func TestAttachBusyRefused(t *testing.T) {
	h := newHarness(t)
	runningPTY(t, h)
	sock := inspectPTY(t, h).Status.AttachSocket
	for i := 0; i < attach.MaxClients; i++ {
		c, err := attach.Dial(sock, attach.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	a := startAttach(t, h, "pty1")
	if code := a.wait(); code != 1 {
		t.Fatalf("exit %d, want 1:\n%q", code, a.output())
	}
	if !strings.Contains(a.output(), "busy") {
		t.Fatalf("no busy message:\n%q", a.output())
	}
	a.termiosRestored()
}

// TestAttachRefusals: a headless session, a missing session, a bad detach
// key and a stdin that is not a terminal are each refused with their code.
func TestAttachRefusals(t *testing.T) {
	h := newHarness(t)
	h.start("hl", []string{"FAKE_SLEEP=30s"})
	a := startAttach(t, h, "hl")
	if code := a.wait(); code != 1 || !strings.Contains(a.output(), "not interactive") {
		t.Fatalf("headless: exit %d:\n%q", code, a.output())
	}
	a.termiosRestored()

	if _, e, code := h.cmd("attach", "nosuch"); code != 127 {
		t.Fatalf("missing session: exit %d: %s", code, e)
	}
	runningPTY(t, h)
	if _, e, code := h.cmd("attach", "pty1", "--detach-key", "ctrl-["); code != 2 || !strings.Contains(e, "ESC") {
		t.Fatalf("ctrl-[: exit %d: %s", code, e)
	}
	// h.cmd gives the command no stdin terminal (/dev/null).
	if _, e, code := h.cmd("attach", "pty1"); code != 2 || !strings.Contains(e, "terminal") {
		t.Fatalf("no tty: exit %d: %s", code, e)
	}
	h.ok("stop", "pty1")
	if _, e, code := h.cmd("attach", "pty1"); code != 1 || !strings.Contains(e, "not running") {
		t.Fatalf("exited session: exit %d: %s", code, e)
	}
}
