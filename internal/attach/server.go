package attach

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Limits the server enforces.
const (
	// MaxClients bounds how many clients may be attached at once; one more
	// is refused with CodeBusy.
	MaxClients = 16
	// ClientBuffer bounds the output queued for one client. A client that
	// falls further behind is disconnected (CodeSlow) rather than allowed to
	// stall the agent or the other clients.
	ClientBuffer = 1 << 20
	// HelloTimeout is how long a new connection has to send its hello.
	HelloTimeout = 5 * time.Second
	// kickTimeout bounds the last writes to a client being disconnected
	// (the queued output and its error or exit frame).
	kickTimeout = time.Second
)

// Config wires a Server to the pty it serves. Every callback may be called
// from several goroutines at once.
type Config struct {
	// Input writes terminal input from a client to the pty. It may block
	// while the agent is not reading; that stalls only the client sending.
	Input func(p []byte) error
	// Resize applies a client's size to the pty and makes the agent redraw.
	// It is called on attach (with the hello's size) and on every resize.
	Resize func(Size)
	// Size reports the pty's current size, for the welcome frame.
	Size func() Size
	// Clients is told the number of attached clients whenever it changes.
	Clients func(n int)
	// Logf logs diagnostics (optional).
	Logf func(format string, args ...any)
}

// Server accepts attach clients on a listener and fans pty output out to
// them. See docs/attach-protocol.md.
type Server struct {
	cfg Config
	l   net.Listener

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
	wg      sync.WaitGroup // client writers
	inputMu sync.Mutex     // keeps each data frame's bytes together on the pty
}

type client struct {
	conn   net.Conn
	out    chan []byte // encoded frames; closed when the client is dropped
	queued int         // bytes in out, guarded by Server.mu
	gone   bool        // dropped; out is closed, guarded by Server.mu
}

// NewServer returns a server for l; call Serve to start accepting.
func NewServer(l net.Listener, cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Clients == nil {
		cfg.Clients = func(int) {}
	}
	return &Server{cfg: cfg, l: l, clients: map[*client]struct{}{}}
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve() {
	for {
		c, err := s.l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.cfg.Logf("attach: accept: %v", err)
			time.Sleep(100 * time.Millisecond) // e.g. EMFILE; do not spin
			continue
		}
		go s.handle(c)
	}
}

func frame(typ byte, payload []byte) []byte {
	b := make([]byte, headerLen+len(payload))
	b[0] = typ
	b[1], b[2], b[3], b[4] = byte(len(payload)>>24), byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	copy(b[headerLen:], payload)
	return b
}

// refuse sends an error frame (best effort, bounded) and closes c.
func refuse(c net.Conn, info ErrorInfo) {
	_ = c.SetWriteDeadline(time.Now().Add(kickTimeout))
	_, _ = c.Write(frame(TypeError, mustJSON(info)))
	_ = c.Close()
}

func (s *Server) handle(conn net.Conn) {
	if uc, ok := conn.(*net.UnixConn); ok {
		owner, err := peerIsOwner(uc)
		if err != nil || !owner {
			s.cfg.Logf("attach: refused a connection from another user (peer credentials: owner=%v err=%v)", owner, err)
			conn.Close()
			return
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(HelloTimeout))
	f, err := ReadFrame(conn)
	if err != nil {
		refuse(conn, ErrorInfo{Code: CodeProtocol, Message: "no hello: " + err.Error()})
		return
	}
	if f.Type != TypeHello {
		refuse(conn, ErrorInfo{Code: CodeProtocol, Message: fmt.Sprintf("first frame has type %d, want hello", f.Type)})
		return
	}
	h, err := parseHello(f.Payload)
	if err != nil {
		refuse(conn, ErrorInfo{Code: CodeProtocol, Message: err.Error()})
		return
	}
	if h.version != Version {
		refuse(conn, ErrorInfo{Code: CodeVersion, Versions: []int{Version},
			Message: fmt.Sprintf("protocol version %d is not supported; this monitor speaks version %d", h.version, Version)})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	size := s.cfg.Size()
	if !h.size.zero() {
		size = h.size
	}
	c := &client{conn: conn, out: make(chan []byte, 256)}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		refuse(conn, ErrorInfo{Code: CodeProtocol, Message: "the session is ending"})
		return
	case len(s.clients) >= MaxClients:
		s.mu.Unlock()
		refuse(conn, ErrorInfo{Code: CodeBusy, Message: fmt.Sprintf("%d clients are already attached", MaxClients)})
		return
	}
	// The welcome is queued before the client can receive any output, and
	// the client is registered before its size is applied, so it sees the
	// whole redraw the resize causes.
	s.enqueueLocked(c, frame(TypeWelcome, encodeU16s(Version, size.Rows, size.Cols)), true)
	s.clients[c] = struct{}{}
	n := len(s.clients)
	s.wg.Add(1)
	s.mu.Unlock()
	go s.writer(c)
	s.cfg.Clients(n)
	if !h.size.zero() {
		s.cfg.Resize(h.size)
	}
	s.reader(c)
}

// reader handles one client's frames until it detaches, disconnects or
// sends something malformed (which drops this client only).
func (s *Server) reader(c *client) {
	for {
		f, err := ReadFrame(c.conn)
		if err != nil {
			if errors.Is(err, ErrFrameTooLarge) || errors.Is(err, io.ErrUnexpectedEOF) {
				s.drop(c, &ErrorInfo{Code: CodeProtocol, Message: err.Error()})
			} else {
				s.drop(c, nil)
			}
			return
		}
		switch f.Type {
		case TypeData:
			if len(f.Payload) == 0 {
				continue
			}
			s.inputMu.Lock()
			err := s.cfg.Input(f.Payload)
			s.inputMu.Unlock()
			if err != nil {
				s.cfg.Logf("attach: write input to the pty: %v", err)
			}
		case TypeResize:
			sz, err := parseSize(f.Payload)
			if err == nil && sz.zero() {
				err = errors.New("resize to a zero size")
			}
			if err != nil {
				s.drop(c, &ErrorInfo{Code: CodeProtocol, Message: err.Error()})
				return
			}
			s.cfg.Resize(sz)
		case TypeDetach:
			s.drop(c, nil)
			return
		default:
			s.drop(c, &ErrorInfo{Code: CodeProtocol, Message: fmt.Sprintf("unexpected frame type %d", f.Type)})
			return
		}
	}
}

// writer sends a client's queued frames until its queue is closed or a
// write fails, then closes the connection.
func (s *Server) writer(c *client) {
	defer s.wg.Done()
	defer c.conn.Close()
	for b := range c.out {
		if _, err := c.conn.Write(b); err != nil {
			s.drop(c, nil)
			for range c.out { // let drop's close end the loop
			}
			return
		}
		s.mu.Lock()
		c.queued -= len(b)
		s.mu.Unlock()
	}
}

// enqueueLocked queues b for c; s.mu is held. Unless force is set, a frame
// that would take the client past ClientBuffer drops it as slow instead.
func (s *Server) enqueueLocked(c *client, b []byte, force bool) {
	if c.gone {
		return
	}
	if !force && c.queued+len(b) > ClientBuffer {
		s.dropLocked(c, &ErrorInfo{Code: CodeSlow, Message: fmt.Sprintf("more than %d bytes of output were waiting for this client", ClientBuffer)})
		return
	}
	select {
	case c.out <- b:
		c.queued += len(b)
	default:
		if force {
			// The queue is full of output; the frame that must arrive
			// (an exit or error) replaces nothing, so the client is cut.
			s.dropLocked(c, nil)
			return
		}
		s.dropLocked(c, &ErrorInfo{Code: CodeSlow, Message: "too many output frames were waiting for this client"})
	}
}

// drop removes c, telling it why when info is set.
func (s *Server) drop(c *client, info *ErrorInfo) {
	s.mu.Lock()
	removed := s.dropLocked(c, info)
	n, closed := len(s.clients), s.closed
	s.mu.Unlock()
	if removed && !closed {
		s.notify(n)
	}
}

// dropLocked removes c from the set and closes its queue; the writer sends
// what is queued (and the error frame, if there is room), bounded by
// kickTimeout, then closes the connection. s.mu is held.
func (s *Server) dropLocked(c *client, info *ErrorInfo) (removed bool) {
	if c.gone {
		return false
	}
	if info != nil {
		s.cfg.Logf("attach: dropping a client: %s: %s", info.Code, info.Message)
		select {
		case c.out <- frame(TypeError, mustJSON(*info)):
		default:
		}
	}
	c.gone = true
	delete(s.clients, c)
	close(c.out)
	_ = c.conn.SetWriteDeadline(time.Now().Add(kickTimeout))
	// The count change is reported by the caller: drop, or Write for the
	// clients a broadcast dropped.
	return true
}

// notify reports the client count (outside s.mu).
func (s *Server) notify(n int) { s.cfg.Clients(n) }

// Write implements io.Writer for the pty's output stream: p is sent to
// every attached client, in frames of at most MaxPayload bytes. It never
// blocks on a client and never fails, so the pty is always drained; a client
// whose queue is full is disconnected.
func (s *Server) Write(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.clients) == 0 || s.closed {
		s.mu.Unlock()
		return len(p), nil
	}
	before := len(s.clients)
	for rest := p; len(rest) > 0; {
		n := min(len(rest), MaxPayload)
		b := frame(TypeData, rest[:n]) // one copy, shared by every client
		rest = rest[n:]
		for c := range s.clients {
			s.enqueueLocked(c, b, false)
		}
	}
	after := len(s.clients)
	s.mu.Unlock()
	if after != before {
		s.notify(after)
	}
	return len(p), nil
}

// Close stops accepting, removes the socket, sends exit (when not nil) to
// every attached client and closes them, waiting at most about kickTimeout
// for their queued output to go out. It is safe to call more than once.
func (s *Server) Close(exit *ExitInfo) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true // Write and drop report no more counts
	_ = s.l.Close()
	for c := range s.clients {
		if exit != nil {
			s.enqueueLocked(c, frame(TypeExit, mustJSON(*exit)), true)
		}
		s.dropLocked(c, nil)
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * kickTimeout):
		s.cfg.Logf("attach: clients still not closed after %s", 2*kickTimeout)
	}
}

// Clients returns how many clients are attached.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

var _ io.Writer = (*Server)(nil)
