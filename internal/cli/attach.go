package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ytjohn/fastener/internal/attach"
	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/session"
)

// Exit codes of `fastener attach` beyond the shared ones: exitFail (1) when
// it cannot attach (the session is not interactive or not running, or the
// monitor refused the client), exitAbnormal (125) when an established
// attachment is lost without the session's outcome, and 128+n when the
// attach process itself got signal n.
const attachUsage = "attach <name> [--detach-key KEY] [--quiet]"

const attachHelp = `usage: fastener ` + attachUsage + `

Connect this terminal to an interactive session's pty. Several terminals may
attach at once; the last to attach or resize sets the size. Keys go to the
agent (ctrl-c included); the detach key leaves the session running. The key
is matched anywhere in the input: bytes after it in the same read (a paste)
are dropped, and it cannot be sent to the agent. Input is queued (up to
1 MiB) while the session is not reading its terminal; beyond that it is
discarded, and the number of bytes discarded is reported on exit. A very fast
multi-megabyte paste into an agent that echoes it can make the monitor drop
this client as too slow (exit 125).

On every way out, the terminal's settings are restored and, when stdout is a
terminal, a reset is written: leave the alternate screen, show the cursor,
turn off mouse reporting and bracketed paste, reset text attributes. Screen
contents are not restored.

Detach keys: ctrl-a … ctrl-z, ctrl-@, ctrl-\, ctrl-], ctrl-^, ctrl-_, or
none. ctrl-[ (ESC) is refused.

Exit status:
  0      detached with the detach key
  N      the session ended while attached: the agent's exit code, as wait
         reports it (128+n for signal n, 125 for a launch failure)
  1      could not attach: the session is not interactive or not running,
         or the monitor refused the connection (busy, version)
  2      usage error, a bad detach key, or stdin is not a terminal
  125    the attachment was lost (connection closed, or the monitor dropped
         this client as too slow)
  127    no such session
  128+n  this attach process got signal n (SIGINT, SIGTERM, SIGHUP); the
         session keeps running. The terminal closing (stdin ends) is
         always reported as SIGHUP: 129

flags:
`

// noDetachKey is parseDetachKey's result for "none".
const noDetachKey = -1

// parseDetachKey turns "ctrl-]", "ctrl-a" … "ctrl-z", "ctrl-@", "ctrl-\",
// "ctrl-^", "ctrl-_" (case-insensitive) into the byte the terminal sends in
// raw mode, or "none" into noDetachKey. ctrl-[ is refused: it is ESC, which
// begins every arrow and function key.
func parseDetachKey(s string) (int, error) {
	k := strings.ToLower(strings.TrimSpace(s))
	if k == "none" {
		return noDetachKey, nil
	}
	rest, ok := strings.CutPrefix(k, "ctrl-")
	if !ok || len(rest) != 1 {
		return 0, fmt.Errorf("detach key %q: want ctrl-<key> (ctrl-a … ctrl-z, ctrl-@, ctrl-\\, ctrl-], ctrl-^, ctrl-_) or none", s)
	}
	c := rest[0]
	switch {
	case c >= 'a' && c <= 'z':
		return int(c - 'a' + 1), nil
	case c == '[':
		return 0, fmt.Errorf("detach key %q is ESC, which starts arrow and function keys; choose another", s)
	case c == '@' || c == '\\' || c == ']' || c == '^' || c == '_':
		return int(c ^ 0x40), nil
	}
	return 0, fmt.Errorf("detach key %q: want ctrl-<key> (ctrl-a … ctrl-z, ctrl-@, ctrl-\\, ctrl-], ctrl-^, ctrl-_) or none", s)
}

// attachEvent is one reason for the attach loop to end.
type attachEvent struct {
	detach bool             // the detach key was pressed
	hangup bool             // stdin ended: the terminal closed
	signal syscall.Signal   // attach itself got a signal
	exit   *attach.ExitInfo // the session ended
	err    error            // the connection failed or the server refused us
}

func cmdAttach(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(attachUsage, stderr)
	keyFlag := fs.String("detach-key", "", `key that detaches (default: config detach_key, else ctrl-]); "none" disables it`)
	quiet := fs.Bool("quiet", false, "do not print the detach hint on attach")
	fs.Usage = func() {
		fmt.Fprint(stderr, attachHelp)
		fs.PrintDefaults()
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	name, ok := oneName(pos, stderr, attachUsage)
	if !ok {
		return exitUsage
	}
	keyText, keySrc := *keyFlag, "--detach-key"
	if keyText == "" {
		cfg, err := config.Load()
		if err != nil {
			return fail(stderr, err)
		}
		keyText, keySrc = cfg.DetachKey, "config detach_key"
	}
	key, err := parseDetachKey(keyText)
	if err != nil {
		fmt.Fprintf(stderr, "fastener: %s: %v\n", keySrc, err)
		return exitUsage
	}

	v, err := session.Load(name, handshakeTimeout())
	if err != nil {
		return fail(stderr, err)
	}
	switch {
	case v.Plan == nil || v.Plan.PTY == nil:
		fmt.Fprintf(stderr, "fastener: session %s is not interactive (it has no terminal to attach to)\n", name)
		return exitFail
	case v.State != session.StateRunning:
		fmt.Fprintf(stderr, "fastener: session %s is %s, not running\n", name, v.State)
		return exitFail
	case v.Status.AttachSocket == "":
		fmt.Fprintf(stderr, "fastener: session %s has no attach socket\n", name)
		return exitFail
	}

	in := int(os.Stdin.Fd())
	if !term.IsTerminal(in) {
		fmt.Fprintln(stderr, "fastener: attach needs a terminal on stdin")
		return exitUsage
	}
	// Signals are caught before the size is read and before raw mode, so
	// none is missed and one arriving at any point still restores the
	// terminal. SIGPIPE is caught so a write to a closed pipe on stdout
	// fails with EPIPE (and takes the error path) instead of killing us.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	pipe := make(chan os.Signal, 1)
	signal.Notify(pipe, syscall.SIGPIPE)
	defer signal.Stop(pipe)
	var size attach.Size
	if cols, rows, err := term.GetSize(in); err == nil && rows > 0 && cols > 0 {
		size = attach.Size{Rows: uint16(rows), Cols: uint16(cols)}
	}

	c, err := attach.Dial(v.Status.AttachSocket, attach.Options{Size: size})
	if err != nil {
		fmt.Fprintf(stderr, "fastener: attach %s: %v\n", name, err)
		return exitFail
	}
	defer c.Close()

	if !*quiet && term.IsTerminal(int(os.Stderr.Fd())) {
		hint := "the detach key is disabled"
		if key != noDetachKey {
			hint = "detach with " + strings.ToLower(keyText)
		}
		fmt.Fprintf(stderr, "[fastener: attached to %s; %s]\n", name, hint)
	}

	old, err := term.MakeRaw(in)
	if err != nil {
		fmt.Fprintf(stderr, "fastener: attach %s: raw mode: %v\n", name, err)
		return exitFail
	}
	// out is a one-slot semaphore around writes to the terminal, so the
	// teardown can stop agent output before restoring (and give up on a
	// write stuck on flow control after a bound).
	out := make(chan struct{}, 1)
	outStopped := false // guarded by holding out
	stdoutTTY := term.IsTerminal(int(os.Stdout.Fd()))
	var once sync.Once
	restore := func() {
		once.Do(func() {
			// Replies to terminal queries the agent sent last (device
			// attributes, version) must not reach the shell as typed input.
			_ = flushInput(in)
			_ = term.Restore(in, old)
			if stdoutTTY {
				_, _ = io.WriteString(stdout, terminalReset)
			}
		})
	}
	defer restore() // also runs while a panic in this goroutine unwinds
	// guard restores the terminal before a panic in another goroutine
	// kills the process (deferred calls here would not run).
	guard := func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
	}

	events := make(chan attachEvent, 4)
	recvDone := make(chan struct{})
	go func() { // server → terminal
		defer close(recvDone)
		defer guard()
		for {
			m, err := c.Recv()
			if err != nil {
				events <- attachEvent{err: err}
				return
			}
			if m.Exit != nil {
				events <- attachEvent{exit: m.Exit}
				return
			}
			out <- struct{}{}
			if outStopped {
				<-out
				return
			}
			_, err = stdout.Write(m.Data)
			<-out
			if err != nil {
				events <- attachEvent{err: fmt.Errorf("write to the terminal: %w", err)}
				return
			}
		}
	}()

	// terminal → server. The reader never blocks on the network, so the
	// detach key is always seen: input goes through a queue of at most
	// inputQueueBytes to the sender, and what does not fit (an agent that
	// is not reading its terminal) is discarded and counted.
	inq := make(chan []byte, 4096)
	var queued, discarded atomic.Int64
	// settling is set once the session has ended: the reader then drops
	// what it reads (late replies to the agent's terminal queries) and
	// stamps lastIn, so settle can wait for the terminal to go quiet.
	var settling atomic.Bool
	var lastIn atomic.Int64
	go func() {
		defer guard()
		for p := range inq {
			err := c.Send(p)
			queued.Add(-int64(len(p))) // after the send, so queued == 0 means all sent
			if err != nil {
				events <- attachEvent{err: err}
				return
			}
		}
	}()
	enqueue := func(p []byte) {
		if len(p) == 0 {
			return
		}
		if queued.Load()+int64(len(p)) > inputQueueBytes {
			discarded.Add(int64(len(p)))
			return
		}
		select {
		case inq <- bytes.Clone(p):
			queued.Add(int64(len(p)))
		default:
			discarded.Add(int64(len(p)))
		}
	}
	go func() {
		defer guard()
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			p := buf[:n]
			if settling.Load() {
				lastIn.Store(time.Now().UnixNano())
				if err != nil {
					return
				}
				continue
			}
			if key != noDetachKey {
				if i := bytes.IndexByte(p, byte(key)); i >= 0 {
					enqueue(p[:i]) // what follows the key in this read is dropped
					events <- attachEvent{detach: true}
					return
				}
			}
			enqueue(p)
			if err != nil {
				events <- attachEvent{hangup: true} // the terminal went away
				return
			}
		}
	}()
	go func() { // SIGWINCH → resize
		defer guard()
		for range winch {
			if cols, rows, err := term.GetSize(in); err == nil && rows > 0 && cols > 0 {
				_ = c.Resize(attach.Size{Rows: uint16(rows), Cols: uint16(cols)})
			}
		}
	}()

	// teardown ends an attachment the session outlives: stop the agent's
	// output reaching the terminal, restore the terminal, then leave the
	// monitor. Every step is bounded; none waits on the network for long.
	teardown := func(flush bool) {
		if flush { // what was typed before the detach key goes out first
			for end := time.Now().Add(teardownWait); queued.Load() > 0 && time.Now().Before(end); {
				time.Sleep(5 * time.Millisecond)
			}
		}
		_ = c.SetReadDeadline(time.Now()) // Recv returns at once
		select {
		case out <- struct{}{}:
			outStopped = true
			<-out
		case <-time.After(teardownWait):
		}
		select {
		case <-recvDone:
		case <-time.After(teardownWait):
		}
		restore()
		_ = c.DetachWithin(teardownWait)
	}
	discardNote := func() {
		if n := discarded.Load(); n > 0 {
			fmt.Fprintf(stderr, "[fastener: %d bytes of input were discarded; the session was not reading its terminal]\n", n)
		}
	}

	var ev attachEvent
	select {
	case sig := <-sigs:
		ev.signal = sig.(syscall.Signal)
	case ev = <-events:
	}
	if ev.hangup {
		// stdin ends when the terminal closes, which also sends SIGHUP; both
		// mean the same thing and are always reported as SIGHUP.
		ev.signal = syscall.SIGHUP
	}
	switch {
	case ev.signal != 0:
		teardown(false)
		fmt.Fprintf(stderr, "\r\n[fastener: %v; detached from %s]\n", ev.signal, name)
		discardNote()
		return 128 + int(ev.signal)
	case ev.detach:
		teardown(true)
		fmt.Fprintf(stderr, "\r\n[fastener: detached from %s]\n", name)
		discardNote()
		return exitOK
	case ev.exit != nil:
		// The agent's last output may have asked the terminal something;
		// let the replies arrive and drop them before restoring.
		settling.Store(true)
		settle(&lastIn)
		restore()
		st := session.Status{EndedBy: ev.exit.EndedBy, ExitCode: ev.exit.ExitCode, Signal: ev.exit.Signal, Error: ev.exit.Error}
		st.AgentError = ev.exit.AgentError
		fmt.Fprintf(stderr, "\r\n[fastener: session %s %s]\n", name, describeExit(st))
		discardNote()
		return exitCodeOf(st)
	default:
		teardown(false)
		var re *attach.RemoteError
		if errors.As(ev.err, &re) {
			fmt.Fprintf(stderr, "\r\n[fastener: the monitor disconnected us from %s: %s]\n", name, re.Message)
		} else {
			fmt.Fprintf(stderr, "\r\n[fastener: lost the connection to %s: %v]\n", name, ev.err)
		}
		discardNote()
		return exitAbnormal
	}
}

// terminalReset undoes the terminal modes a full-screen agent commonly
// sets and that termios does not cover: leave the alternate screen, show
// the cursor, turn off mouse reporting and bracketed paste, reset text
// attributes. Written to stdout, when it is a terminal, after the restore.
const terminalReset = "\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[0m"

// inputQueueBytes bounds the input attach holds for a session that is not
// reading its terminal; more is discarded (and reported).
const inputQueueBytes = 1 << 20

// settleQuiet and settleMax bound how long attach waits, after the session
// ends, for the terminal's replies to the agent's last queries: until no
// input has arrived for settleQuiet, and never longer than settleMax.
const (
	settleQuiet = 50 * time.Millisecond
	settleMax   = 300 * time.Millisecond
)

func settle(lastIn *atomic.Int64) {
	start := time.Now()
	for {
		now := time.Now()
		if now.Sub(start) >= settleMax {
			return
		}
		quietSince := start
		if t := lastIn.Load(); t > 0 && time.Unix(0, t).After(start) {
			quietSince = time.Unix(0, t)
		}
		if now.Sub(quietSince) >= settleQuiet {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// teardownWait bounds each step of leaving an attachment.
const teardownWait = 500 * time.Millisecond

// describeExit says how a session ended, for attach's last line.
func describeExit(s session.Status) string {
	var how string
	switch {
	case s.Signal != "":
		how = "was killed by " + s.Signal
	case s.ExitCode != nil:
		how = fmt.Sprintf("exited %d", *s.ExitCode)
	default:
		how = "ended"
	}
	switch s.EndedBy {
	case "", session.EndedByExit:
	case session.EndedByLaunchFailed:
		how = "failed to launch"
	default:
		how += " (ended by " + s.EndedBy + ")"
	}
	if s.Error != "" {
		how += ": " + s.Error
	}
	return how
}
