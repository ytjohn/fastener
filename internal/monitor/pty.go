package monitor

import (
	"os"
	"sync"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/ytjohn/fastener/internal/attach"
	"github.com/ytjohn/fastener/internal/session"
)

// ptyIO is an interactive generation's terminal: the pty the agent runs on
// (its controlling terminal) and the attach server in front of it (design
// §8.2, docs/attach-protocol.md).
//
// Output: the monitor reads the master and passes every chunk to the attach
// server (which never blocks: a client that falls behind is dropped) and to
// the normal pump, which writes output.log and feeds the plan's parser. So
// the pty is drained whether or not anyone is attached.
type ptyIO struct {
	m      *monitor
	master *os.File
	slave  *os.File // the agent's stdin, stdout and stderr; closed after the spawn
	srv    *attach.Server
	path   string // where the attach socket is bound

	// sizeMu serializes resizes and guards the same-size nudge (see
	// resize): gen counts resizes, so a delayed restore that a later resize
	// overtook does nothing; want is the size a client asked for, reported
	// while the pty is still one column narrower.
	sizeMu  sync.Mutex
	gen     uint64
	nudging bool
	want    attach.Size
}

// nudgeDelay is how long a same-size attach keeps the pty one column
// narrower before restoring the client's size: long enough for the agent to
// handle the first SIGWINCH on its own, so it sees two real changes.
var nudgeDelay = 100 * time.Millisecond

// openPTY opens the pty at the plan's size and starts the attach server.
func (m *monitor) openPTY() (*ptyIO, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	p := &ptyIO{m: m, master: master, slave: slave}
	rows, cols := m.plan.PTY.Size()
	if err := p.setSize(attach.Size{Rows: rows, Cols: cols}); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	l, path, err := attach.Listen(string(m.d))
	if err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	p.path = path
	p.srv = attach.NewServer(l, attach.Config{
		Input:   func(b []byte) error { _, err := master.Write(b); return err },
		Resize:  p.resize,
		Size:    p.reportedSize,
		Clients: p.clients,
		Logf:    m.log.Printf,
	})
	go p.srv.Serve()
	return p, nil
}

// control runs f on the master's fd while holding a reference to it, so a
// concurrent Close cannot make f act on a reused descriptor.
func (p *ptyIO) control(f func(fd int) error) error {
	rc, err := p.master.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func (p *ptyIO) setSize(s attach.Size) error {
	return p.control(func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: s.Rows, Col: s.Cols})
	})
}

func (p *ptyIO) size() attach.Size {
	var s attach.Size
	_ = p.control(func(fd int) error {
		ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		if err == nil {
			s = attach.Size{Rows: ws.Row, Cols: ws.Col}
		}
		return err
	})
	return s
}

// resize applies a client's size (the last client to attach or resize
// wins). Setting a different size makes the kernel send SIGWINCH to the
// pty's foreground process group, and the agent redraws.
//
// An attach at the size the pty already has must redraw too, but a bare
// SIGWINCH is not enough: Node emits 'resize' only when the size changed,
// and Ink writes nothing for a frame identical to the last one, so Claude
// Code (and other Ink TUIs) would leave a fresh client's screen blank. So a
// same-size resize nudges: the pty is set one column narrower (the layout
// changes, so the agent redraws), then back to the client's size after
// nudgeDelay (it redraws again, at the right width).
func (p *ptyIO) resize(s attach.Size) {
	p.sizeMu.Lock()
	defer p.sizeMu.Unlock()
	p.gen++
	p.want = s
	p.nudging = false
	if p.size() != s {
		if err := p.setSize(s); err != nil {
			p.m.log.Printf("pty resize to %dx%d: %v", s.Rows, s.Cols, err)
		}
		return
	}
	n := s
	if n.Cols > 1 {
		n.Cols--
	} else {
		n.Cols++
	}
	if err := p.setSize(n); err != nil {
		p.m.log.Printf("pty resize to %dx%d: %v", n.Rows, n.Cols, err)
		return
	}
	p.nudging = true
	gen := p.gen
	time.AfterFunc(nudgeDelay, func() {
		p.sizeMu.Lock()
		defer p.sizeMu.Unlock()
		if p.gen != gen {
			return // a later resize set the size
		}
		p.nudging = false
		_ = p.setSize(s) // fails only once the pty is closed
	})
}

// reportedSize is the pty's size as a client should see it: the size asked
// for while a nudge holds the pty one column narrower.
func (p *ptyIO) reportedSize() attach.Size {
	p.sizeMu.Lock()
	defer p.sizeMu.Unlock()
	if p.nudging {
		return p.want
	}
	return p.size()
}

// clients records the attached-client count while the agent runs; the
// flush loop writes it. (The running record sets it from srv.Clients, so a
// client that attached during starting is counted too.)
func (p *ptyIO) clients(n int) {
	m := p.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.final || m.st.State != session.StateRunning {
		return
	}
	m.st.AttachedClients = &n
	m.dirty = true
}

// close ends the attach server: the socket is removed and every attached
// client is sent exit (when the session has an outcome) and closed.
func (p *ptyIO) close() {
	m := p.m
	m.mu.Lock()
	var exit *attach.ExitInfo
	if m.st.State == session.StateExited {
		exit = &attach.ExitInfo{EndedBy: m.st.EndedBy, ExitCode: clonePtr(m.st.ExitCode), Signal: m.st.Signal, Error: m.st.Error,
			AgentError: clonePtr(m.st.AgentError)}
	}
	m.mu.Unlock()
	p.srv.Close(exit)
	p.slave.Close() // no-op after the spawn closed it
}
