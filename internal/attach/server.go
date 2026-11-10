package attach

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
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
	// Input writes one client frame of terminal input to the pty. It is
	// called from a single goroutine, so it may block while the agent is not
	// reading: input then queues (inputQueue frames) and, once that is full,
	// clients sending more input wait; resizes and detaches never do.
	Input func(p []byte) error
	// Resize applies a client's size to the pty and makes the agent redraw.
	// It is called on attach (with the hello's size) and on every resize.
	Resize func(Size)
	// Size reports the pty's current size, for the welcome frame.
	Size func() Size
	// Clients is told the number of attached clients after it changes
	// (serialized; the last call carries the current count).
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

	// input feeds the one goroutine that writes to the pty, a frame at a
	// time, so each frame's bytes stay together and a pty write that blocks
	// (the agent is not reading) holds up only clients sending input, never
	// a resize or a detach. quit ends it (and any reader waiting on it).
	input chan []byte
	quit  chan struct{}

	// notifyMu serializes count reports; each reads the count when it is
	// delivered, so the last report always carries the current count.
	notifyMu sync.Mutex

	// ownerUID is the only uid allowed to connect: this process's (tests
	// change it to exercise the refusal).
	ownerUID atomic.Int64
}

// writeBatch bounds the bytes of one writev to a client.
const writeBatch = 64 << 10

// inputQueue is how many input frames may wait for the pty writer. A client
// sending input while it is full waits; other frames are not affected.
const inputQueue = 64

type client struct {
	conn net.Conn
	wake chan struct{} // cap 1: the writer has something to do
	done chan struct{} // closed when the client is dropped

	// Guarded by Server.mu.
	pending [][]byte // encoded frames not yet taken by the writer
	queued  int      // bytes in pending plus those being written
	closing bool     // dropped: nothing more is queued; the writer ends once pending is sent
}

// NewServer returns a server for l; call Serve to start accepting.
func NewServer(l net.Listener, cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Clients == nil {
		cfg.Clients = func(int) {}
	}
	s := &Server{cfg: cfg, l: l, clients: map[*client]struct{}{},
		input: make(chan []byte, inputQueue), quit: make(chan struct{})}
	s.ownerUID.Store(int64(os.Getuid()))
	go s.ptyWriter()
	return s
}

// ptyWriter writes queued input to the pty until the server closes. A write
// that blocks forever (an agent that never reads) is abandoned at exit.
func (s *Server) ptyWriter() {
	for {
		select {
		case p := <-s.input:
			if err := s.cfg.Input(p); err != nil {
				s.cfg.Logf("attach: write input to the pty: %v", err)
			}
		case <-s.quit:
			return
		}
	}
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
		uid, err := peerUIDOf(uc)
		if owner := int(s.ownerUID.Load()); err != nil || uid != owner {
			s.cfg.Logf("attach: refused a connection from uid %d (err %v); only uid %d may attach", uid, err, owner)
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

	c := &client{conn: conn, wake: make(chan struct{}, 1), done: make(chan struct{})}
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
	// Under s.mu, so no output is fanned out meanwhile: apply the size,
	// queue the welcome with the size now in effect, and register the
	// client. Output (including the redraw the resize causes) can only be
	// queued for it after the welcome.
	if !h.size.zero() {
		s.cfg.Resize(h.size)
	}
	size := s.cfg.Size()
	s.enqueueLocked(c, frame(TypeWelcome, encodeU16s(Version, size.Rows, size.Cols)))
	s.clients[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go s.writer(c)
	s.notify()
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
			select {
			case s.input <- f.Payload:
			case <-c.done:
				return
			case <-s.quit:
				return
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
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				s.cfg.Resize(sz)
			}
		case TypeDetach:
			s.drop(c, nil)
			return
		default:
			s.drop(c, &ErrorInfo{Code: CodeProtocol, Message: fmt.Sprintf("unexpected frame type %d", f.Type)})
			return
		}
	}
}

// writer sends a client's queued frames (several at once, with writev)
// until the client is dropped and everything queued has gone out, or a
// write fails; then it closes the connection.
func (s *Server) writer(c *client) {
	defer s.wg.Done()
	defer c.conn.Close()
	for {
		s.mu.Lock()
		for len(c.pending) == 0 && !c.closing {
			s.mu.Unlock()
			<-c.wake
			s.mu.Lock()
		}
		// Take at most about writeBatch bytes: queued counts a batch until
		// it is written, so a big batch would make a client that keeps up
		// look further behind than it is.
		k, n := 0, 0
		for k < len(c.pending) && (k == 0 || n+len(c.pending[k]) <= writeBatch) {
			n += len(c.pending[k])
			k++
		}
		batch := slices.Clone(c.pending[:k])
		c.pending = c.pending[k:]
		if len(c.pending) == 0 {
			c.pending = nil // let the backing array go
		}
		closing := c.closing
		s.mu.Unlock()
		if len(batch) == 0 && closing {
			return
		}
		bufs := net.Buffers(batch)
		if _, err := bufs.WriteTo(c.conn); err != nil {
			s.drop(c, nil)
			return
		}
		s.mu.Lock()
		c.queued -= n
		s.mu.Unlock()
	}
}

func (c *client) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// enqueueLocked queues b for c; s.mu is held. Only bytes are counted: a
// client with more than ClientBuffer bytes waiting is dropped as slow, its
// waiting output discarded so the slow error frame is next on the wire.
func (s *Server) enqueueLocked(c *client, b []byte) {
	if c.closing {
		return
	}
	if c.queued+len(b) > ClientBuffer {
		s.dropLocked(c, &ErrorInfo{Code: CodeSlow, Message: fmt.Sprintf("more than %d bytes of output were waiting for this client", ClientBuffer)}, true)
		return
	}
	c.pending = append(c.pending, b)
	c.queued += len(b)
	c.poke()
}

// drop removes c, telling it why when info is set.
func (s *Server) drop(c *client, info *ErrorInfo) {
	s.mu.Lock()
	removed := s.dropLocked(c, info, false)
	s.mu.Unlock()
	if removed {
		s.notify()
	}
}

// dropLocked removes c from the set; s.mu is held. Its writer sends what is
// queued (all of it, or with discard only the error frame) and closes the
// connection, all bounded by kickTimeout.
func (s *Server) dropLocked(c *client, info *ErrorInfo, discard bool) (removed bool) {
	if c.closing {
		return false
	}
	if discard {
		for _, b := range c.pending {
			c.queued -= len(b) // a batch being written still counts until it is done
		}
		c.pending = nil
	}
	if info != nil {
		s.cfg.Logf("attach: dropping a client: %s: %s", info.Code, info.Message)
		c.pending = append(c.pending, frame(TypeError, mustJSON(*info)))
	}
	c.closing = true
	delete(s.clients, c)
	close(c.done)
	_ = c.conn.SetWriteDeadline(time.Now().Add(kickTimeout))
	c.poke()
	return true
}

// notify reports the current client count. Reports are serialized and each
// reads the count as it is delivered, so they can never leave a stale count
// behind. Nothing is reported once the server is closing.
func (s *Server) notify() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.mu.Lock()
	n, closed := len(s.clients), s.closed
	s.mu.Unlock()
	if !closed {
		s.cfg.Clients(n)
	}
}

// Write implements io.Writer for the pty's output stream: p is sent to
// every attached client, in frames of at most MaxPayload bytes. It never
// blocks on a client and never fails, so the pty is always drained; a client
// too far behind is disconnected.
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
			s.enqueueLocked(c, b)
		}
	}
	after := len(s.clients)
	s.mu.Unlock()
	if after != before {
		s.notify()
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
	s.closed = true // no more counts, resizes or input
	close(s.quit)
	_ = s.l.Close()
	for c := range s.clients {
		if exit != nil {
			// Past ClientBuffer if need be: the exit must follow the output.
			c.pending = append(c.pending, frame(TypeExit, mustJSON(*exit)))
		}
		s.dropLocked(c, nil, false)
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
