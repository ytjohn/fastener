package monitor

import (
	"os"
	"sync"
	"syscall"

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

	// sigMu guards pgid and reaped, so a SIGWINCH is sent to the agent's
	// group only between the spawn and the reap: until the agent is reaped
	// its zombie pins the pgid, so the signal cannot reach a stranger.
	sigMu  sync.Mutex
	pgid   int
	reaped bool
}

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
		Size:    p.size,
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
// pty's foreground process group; when the size is unchanged it does not,
// so the monitor sends SIGWINCH itself: an attach always forces a redraw.
func (p *ptyIO) resize(s attach.Size) {
	before := p.size()
	if err := p.setSize(s); err != nil {
		p.m.log.Printf("pty resize to %dx%d: %v", s.Rows, s.Cols, err)
		return
	}
	if before == s {
		p.winch()
	}
}

// winch sends SIGWINCH to the pty's foreground process group (TIOCGPGRP on
// the master, which Linux allows), else to the agent's process group (macOS
// answers TIOCGPGRP only on a process's own controlling terminal).
func (p *ptyIO) winch() {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	if p.pgid == 0 || p.reaped {
		return
	}
	pg := p.pgid
	_ = p.control(func(fd int) error {
		fg, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if err == nil && fg > 0 {
			pg = fg
		}
		return err
	})
	_ = syscall.Kill(-pg, syscall.SIGWINCH)
}

func (p *ptyIO) spawned(pgid int) {
	p.sigMu.Lock()
	p.pgid = pgid
	p.sigMu.Unlock()
}

// reaping is called just before the agent is reaped; no signal follows it.
func (p *ptyIO) reaping() {
	p.sigMu.Lock()
	p.reaped = true
	p.sigMu.Unlock()
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
		exit = &attach.ExitInfo{EndedBy: m.st.EndedBy, ExitCode: clonePtr(m.st.ExitCode), Signal: m.st.Signal, Error: m.st.Error}
	}
	m.mu.Unlock()
	p.srv.Close(exit)
	p.slave.Close() // no-op after the spawn closed it
}
