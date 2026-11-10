package attach

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePTY records what the server asks of the pty.
type fakePTY struct {
	mu      sync.Mutex
	input   bytes.Buffer
	resizes []Size
	size    Size
	counts  []int
}

func (f *fakePTY) config() Config {
	return Config{
		Input: func(p []byte) error { f.mu.Lock(); f.input.Write(p); f.mu.Unlock(); return nil },
		Resize: func(s Size) {
			f.mu.Lock()
			f.resizes = append(f.resizes, s)
			if s.Rows < 1000 { // larger sizes "fail" and leave the size alone
				f.size = s
			}
			f.mu.Unlock()
		},
		Size:    func() Size { f.mu.Lock(); defer f.mu.Unlock(); return f.size },
		Clients: func(n int) { f.mu.Lock(); f.counts = append(f.counts, n); f.mu.Unlock() },
	}
}

func (f *fakePTY) lastCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.counts) == 0 {
		return 0
	}
	return f.counts[len(f.counts)-1]
}

// shortDir is a temp dir with a short path, so the socket fits in sun_path
// on macOS too (t.TempDir paths there are long).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "fa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func startServer(t *testing.T) (*Server, *fakePTY, string) {
	t.Helper()
	dir := shortDir(t)
	l, path, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePTY{size: Size{24, 80}}
	cfg := f.config()
	cfg.Logf = t.Logf
	s := NewServer(l, cfg)
	go s.Serve()
	t.Cleanup(func() { s.Close(nil) })
	return s, f, path
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// recvUntil reads data messages until their concatenation contains want.
func recvUntil(t *testing.T, c *Client, want string) string {
	t.Helper()
	var got strings.Builder
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	for !strings.Contains(got.String(), want) {
		m, err := c.Recv()
		if err != nil {
			t.Fatalf("waiting for %q (have %q): %v", want, got.String(), err)
		}
		got.Write(m.Data)
	}
	return got.String()
}

func TestFrameRoundTripAndLimits(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, TypeData, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got := b.Bytes(); !bytes.Equal(got, []byte{TypeData, 0, 0, 0, 2, 'h', 'i'}) {
		t.Fatalf("wire bytes % x", got)
	}
	f, err := ReadFrame(&b)
	if err != nil || f.Type != TypeData || string(f.Payload) != "hi" {
		t.Fatalf("ReadFrame = %+v, %v", f, err)
	}
	if err := WriteFrame(io.Discard, TypeData, make([]byte, MaxPayload+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized write: %v", err)
	}
	big := []byte{TypeData, 0, 1, 0, 1} // 65537
	if _, err := ReadFrame(bytes.NewReader(big)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized read: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{TypeData, 0, 0, 0, 5, 'a'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("empty read: %v", err)
	}
}

func TestSocketModeAndRemoval(t *testing.T) {
	s, _, path := startServer(t)
	if filepath.Base(path) != SocketName {
		t.Fatalf("path %s", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	s.Close(nil)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed on close: %v", err)
	}
}

func TestListenReplacesStaleSocketRefusesOtherFiles(t *testing.T) {
	dir := shortDir(t)
	l, path, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.UnixListener.Close() // a monitor that died leaves its socket behind
	l2, _, err := Listen(dir)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	l2.Close()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Listen(dir); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file at the socket path: %v", err)
	}
}

func TestSocketPathFallback(t *testing.T) {
	root := shortDir(t)
	old := FallbackRoot
	FallbackRoot = root
	t.Cleanup(func() { FallbackRoot = old })

	long := filepath.Join(shortDir(t), strings.Repeat("d", 120))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	p, fb := SocketPath(long)
	if !fb || len(p) > maxSocketPath || !strings.HasPrefix(p, filepath.Join(root, "fastener-")) {
		t.Fatalf("SocketPath = %s, %v", p, fb)
	}
	if p2, _ := SocketPath(long); p2 != p {
		t.Fatalf("not deterministic: %s vs %s", p, p2)
	}
	l, got, err := Listen(long)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got != p {
		t.Fatalf("listened at %s, want %s", got, p)
	}
	conn, err := net.Dial("unix", got)
	if err != nil {
		t.Fatalf("dial fallback socket: %v", err)
	}
	conn.Close()

	// A fallback directory with group/other access is refused.
	if err := os.Chmod(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Listen(long + "x"); err == nil {
		// the other long dir maps to the same fallback directory
		t.Fatal("fallback directory with mode 0755 accepted")
	}
	_ = os.Chmod(filepath.Dir(p), 0o700)
}

func TestHandshakeAndIO(t *testing.T) {
	s, f, path := startServer(t)
	c, err := Dial(path, Options{Size: Size{40, 120}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Welcome.Version != Version || c.Welcome.Size != (Size{40, 120}) {
		t.Fatalf("welcome %+v", c.Welcome)
	}
	waitUntil(t, "resize on attach", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.resizes) == 1 })
	if err := c.Send([]byte("typed")); err != nil {
		t.Fatal(err)
	}
	if err := c.Resize(Size{10, 20}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "input and resize", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.input.String() == "typed" && len(f.resizes) == 2 && f.resizes[1] == (Size{10, 20})
	})
	if _, err := s.Write([]byte("output")); err != nil {
		t.Fatal(err)
	}
	recvUntil(t, c, "output")
	if n := f.lastCount(); n != 1 {
		t.Fatalf("client count %d", n)
	}
	if err := c.Detach(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "detach", func() bool { return s.Clients() == 0 && f.lastCount() == 0 })
}

func TestAttachWithoutSizeDoesNotResize(t *testing.T) {
	_, f, path := startServer(t)
	c, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Welcome.Size != (Size{24, 80}) {
		t.Fatalf("welcome %+v", c.Welcome)
	}
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resizes) != 0 {
		t.Fatalf("resized: %v", f.resizes)
	}
}

func TestWrongVersionRejected(t *testing.T) {
	s, _, path := startServer(t)
	_, err := Dial(path, Options{Version: 99})
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeVersion || len(re.Versions) != 1 || re.Versions[0] != Version {
		t.Fatalf("Dial(v99) = %v", err)
	}
	if s.Clients() != 0 {
		t.Fatal("rejected client counted")
	}
}

func TestNonHelloFirstFrameRejected(t *testing.T) {
	_, _, path := startServer(t)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteFrame(conn, TypeData, []byte("x")); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFrame(conn)
	if err != nil || f.Type != TypeError || !strings.Contains(string(f.Payload), CodeProtocol) {
		t.Fatalf("got %+v %v", f, err)
	}
}

// TestMalformedFrameDropsOnlyThatClient: a client sending an oversized
// header or an unknown type is closed; the others keep receiving.
func TestMalformedFrameDropsOnlyThatClient(t *testing.T) {
	s, _, path := startServer(t)
	good, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	for _, bad := range [][]byte{
		{TypeData, 0xff, 0xff, 0xff, 0xff}, // over MaxPayload
		{42, 0, 0, 0, 0},                   // unknown type
		{TypeResize, 0, 0, 0, 1, 0},        // resize of the wrong length
	} {
		c, err := Dial(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		c.wmu.Lock()
		_, _ = c.conn.Write(bad)
		c.wmu.Unlock()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, err := c.Recv()
			if err == nil {
				continue
			}
			var re *RemoteError
			if !errors.As(err, &re) || re.Code != CodeProtocol {
				t.Fatalf("bad frame % x: %v", bad, err)
			}
			break
		}
		c.Close()
	}
	waitUntil(t, "bad clients gone", func() bool { return s.Clients() == 1 })
	s.Write([]byte("still here"))
	recvUntil(t, good, "still here")
}

func TestTwoClientsBothGetOutput(t *testing.T) {
	s, f, path := startServer(t)
	a, err := Dial(path, Options{Size: Size{30, 100}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Dial(path, Options{Size: Size{50, 132}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	waitUntil(t, "two clients", func() bool { return s.Clients() == 2 })
	s.Write([]byte("hello both"))
	recvUntil(t, a, "hello both")
	recvUntil(t, b, "hello both")
	// Last to attach wins the size.
	if got := f.config().Size(); got != (Size{50, 132}) {
		t.Fatalf("size %v", got)
	}
	a.Send([]byte("from a "))
	waitUntil(t, "a's input", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.input.String() == "from a " })
	b.Send([]byte("from b"))
	waitUntil(t, "b's input", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.input.String() == "from a from b" })
}

// TestSlowClientIsDropped: a client that never reads is disconnected once
// its queue is full; Write never blocks and a reading client gets it all.
func TestSlowClientIsDropped(t *testing.T) {
	s, _, path := startServer(t)
	slow, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fast, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	waitUntil(t, "two clients", func() bool { return s.Clients() == 2 })

	// The fast client reads everything; the producer keeps at most 512 KiB
	// ahead of it (as an agent's output is paced by the agent), while the
	// slow client never reads at all.
	var got atomic.Int64
	readDone := make(chan error, 1)
	go func() {
		for {
			m, err := fast.Recv()
			if err != nil {
				readDone <- err
				return
			}
			if got.Add(int64(len(m.Data))) >= 8<<20 {
				readDone <- nil
				return
			}
		}
	}()
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	start := time.Now()
	for i := 0; i < 256; i++ { // 8 MiB, far more than ClientBuffer plus socket buffers
		for int64(i*len(chunk))-got.Load() > 512<<10 {
			if time.Since(start) > 10*time.Second {
				t.Fatalf("fast client stalled at %d bytes", got.Load())
			}
			time.Sleep(time.Millisecond)
		}
		s.Write(chunk)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("fast client: %v (after %d bytes)", err, got.Load())
	}
	waitUntil(t, "slow client dropped", func() bool { return s.Clients() == 1 })
}

func TestBusy(t *testing.T) {
	s, _, path := startServer(t)
	var cs []*Client
	defer func() {
		for _, c := range cs {
			c.Close()
		}
	}()
	for i := 0; i < MaxClients; i++ {
		c, err := Dial(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	waitUntil(t, "all attached", func() bool { return s.Clients() == MaxClients })
	_, err := Dial(path, Options{})
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeBusy {
		t.Fatalf("extra client: %v", err)
	}
}

func TestCloseSendsExitAndRemovesSocket(t *testing.T) {
	s, _, path := startServer(t)
	c, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitUntil(t, "attached", func() bool { return s.Clients() == 1 })
	s.Write([]byte("last words"))
	code := 3
	s.Close(&ExitInfo{EndedBy: "exit", ExitCode: &code})
	recvUntil(t, c, "last words")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := c.Recv()
	if err != nil || m.Type != TypeExit || m.Exit.EndedBy != "exit" || m.Exit.ExitCode == nil || *m.Exit.ExitCode != 3 {
		t.Fatalf("exit message %+v %v", m, err)
	}
	if _, err := c.Recv(); err != io.EOF {
		t.Fatalf("after exit: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still there: %v", err)
	}
	if _, err := Dial(path, Options{}); err == nil {
		t.Fatal("dial after close succeeded")
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	dir := shortDir(t)
	l, path, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, err := Listen(dir); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second Listen on a live socket: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the live socket was removed: %v", err)
	}
}

// TestCloseKeepsASuccessorsSocket: an old listener whose socket file was
// replaced (a new generation's monitor) does not remove the new one.
func TestCloseKeepsASuccessorsSocket(t *testing.T) {
	dir := shortDir(t)
	old, path, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path) // as if it had gone stale and been replaced
	succ, _, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer succ.Close()
	old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("closing the old listener removed its successor's socket: %v", err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("successor unreachable: %v", err)
	}
	conn.Close()
}

func TestFallbackDirChecks(t *testing.T) {
	long := func(t *testing.T) string {
		return filepath.Join(shortDir(t), strings.Repeat("d", 120))
	}
	useRoot := func(t *testing.T) string {
		root := shortDir(t)
		old := FallbackRoot
		FallbackRoot = root
		t.Cleanup(func() { FallbackRoot = old })
		return filepath.Join(root, "fastener-"+strconv.Itoa(os.Getuid()))
	}
	t.Run("symlink", func(t *testing.T) {
		fb := useRoot(t)
		target := shortDir(t)
		if err := os.Chmod(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, fb); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Listen(long(t)); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("symlinked fallback directory: %v", err)
		}
	})
	for _, mode := range []os.FileMode{0o750, 0o705, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			fb := useRoot(t)
			if err := os.Mkdir(fb, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(fb, mode); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Listen(long(t)); err == nil || !strings.Contains(err.Error(), "no group or other access") {
				t.Fatalf("fallback directory with mode %v: %v", mode, err)
			}
		})
	}
	t.Run("ok", func(t *testing.T) {
		fb := useRoot(t)
		l, path, err := Listen(long(t))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		fi, err := os.Stat(fb)
		if err != nil || fi.Mode().Perm() != 0o700 || filepath.Dir(path) != fb {
			t.Fatalf("fallback dir %v %v, socket %s", fi.Mode(), err, path)
		}
	})
}

// TestPeerOfAnotherUserRefused: a connection whose peer credentials name
// another uid is closed without a welcome.
func TestPeerOfAnotherUserRefused(t *testing.T) {
	s, _, path := startServer(t)
	s.ownerUID.Store(int64(os.Getuid() + 1))
	// The server closes at once: the hello's write or the welcome's read
	// fails, depending on timing; either way there is no welcome.
	if c, err := Dial(path, Options{}); err == nil {
		c.Close()
		t.Fatal("a stranger was welcomed")
	}
	if s.Clients() != 0 {
		t.Fatal("stranger counted as a client")
	}
	s.ownerUID.Store(int64(os.Getuid()))
	c, err := Dial(path, Options{})
	if err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	c.Close()
}

// TestStuckInputDoesNotBlockOthers: while the pty write of one client's
// input is stuck, another client's resize and detach still go through, and
// output still flows.
func TestStuckInputDoesNotBlockOthers(t *testing.T) {
	dir := shortDir(t)
	l, path, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePTY{size: Size{24, 80}}
	cfg := f.config()
	stuck := make(chan struct{})
	entered := make(chan struct{}, 1)
	cfg.Input = func(p []byte) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-stuck
		return nil
	}
	s := NewServer(l, cfg)
	go s.Serve()
	t.Cleanup(func() { close(stuck); s.Close(nil) })

	a, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// a pastes far more than the input queue holds; its reader ends up
	// waiting, and the pty writer is stuck in Input.
	go func() {
		for i := 0; i < 4*inputQueue; i++ {
			if a.Send([]byte("paste")) != nil {
				return
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("input never reached the pty")
	}
	if err := b.Resize(Size{33, 99}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "b's resize despite a's stuck input", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.resizes) > 0 && f.resizes[len(f.resizes)-1] == (Size{33, 99})
	})
	s.Write([]byte("output flows"))
	recvUntil(t, b, "output flows")
	if err := b.Detach(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "b's detach despite a's stuck input", func() bool { return s.Clients() == 1 })
}

// TestClientCountNeverStale: with attaches and detaches racing and a slow
// Clients callback, the last count reported is the real one.
func TestClientCountNeverStale(t *testing.T) {
	dir := shortDir(t)
	l, path, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	last := -1
	cfg := (&fakePTY{size: Size{24, 80}}).config()
	cfg.Clients = func(n int) {
		time.Sleep(time.Duration(n%3) * time.Millisecond) // reorder unserialized reports
		mu.Lock()
		last = n
		mu.Unlock()
	}
	s := NewServer(l, cfg)
	go s.Serve()
	defer s.Close(nil)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				c, err := Dial(path, Options{})
				var re *RemoteError
				if errors.As(err, &re) && re.Code == CodeBusy {
					continue // detaches are processed asynchronously
				}
				if err != nil {
					t.Error(err)
					return
				}
				c.Detach()
			}
		}()
	}
	wg.Wait()
	waitUntil(t, "no clients", func() bool { return s.Clients() == 0 })
	time.Sleep(100 * time.Millisecond) // let the last reports land
	mu.Lock()
	defer mu.Unlock()
	if last != 0 {
		t.Fatalf("last reported count %d, want 0", last)
	}
}

// TestWelcomeReportsSizeInEffect: when applying the hello's size fails the
// welcome reports the pty's actual size.
func TestWelcomeReportsSizeInEffect(t *testing.T) {
	_, _, path := startServer(t)
	c, err := Dial(path, Options{Size: Size{2000, 80}}) // the fake refuses rows >= 1000
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Welcome.Size != (Size{24, 80}) {
		t.Fatalf("welcome %+v, want the unchanged 24x80", c.Welcome.Size)
	}
}

// TestManySmallFramesDoNotDrop: only bytes count against ClientBuffer, so a
// burst of tiny chunks to a client that is not reading yet is kept.
func TestManySmallFramesDoNotDrop(t *testing.T) {
	s, _, path := startServer(t)
	c, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitUntil(t, "attached", func() bool { return s.Clients() == 1 })
	for i := 0; i < 5000; i++ {
		s.Write([]byte("tiny-"))
	}
	s.Write([]byte("END"))
	recvUntil(t, c, "END")
	if s.Clients() != 1 {
		t.Fatal("client dropped")
	}
}

// TestSlowClientIsToldWhy: a client dropped as slow that then starts
// reading gets the slow error frame, not a bare EOF.
func TestSlowClientIsToldWhy(t *testing.T) {
	s, _, path := startServer(t)
	c, err := Dial(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitUntil(t, "attached", func() bool { return s.Clients() == 1 })
	chunk := bytes.Repeat([]byte("y"), 32<<10)
	for i := 0; s.Clients() == 1 && i < 1024; i++ {
		s.Write(chunk)
	}
	if s.Clients() != 0 {
		t.Fatal("never dropped")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, err := c.Recv()
		if err == nil {
			continue
		}
		var re *RemoteError
		if !errors.As(err, &re) || re.Code != CodeSlow {
			t.Fatalf("slow client got %v, want the slow error", err)
		}
		return
	}
}
