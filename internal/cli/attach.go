package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

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
agent (ctrl-c included); the detach key leaves the session running.

Detach keys: ctrl-a … ctrl-z, ctrl-@, ctrl-\, ctrl-], ctrl-^, ctrl-_, or
none. ctrl-[ (ESC) is refused.

Exit status:
  0      detached (the detach key, or the terminal closed)
  N      the session ended while attached: the agent's exit code, as wait
         reports it (128+n for signal n, 125 for a launch failure)
  1      could not attach: the session is not interactive or not running,
         or the monitor refused the connection (busy, version)
  2      usage error, a bad detach key, or stdin is not a terminal
  125    the attachment was lost (connection closed, or the monitor dropped
         this client as too slow)
  127    no such session
  128+n  this attach process got signal n (SIGINT, SIGTERM, SIGHUP); the
         session keeps running

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
	detach bool             // the detach key was pressed (or stdin ended)
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
	var size attach.Size
	if cols, rows, err := term.GetSize(in); err == nil && rows > 0 && cols > 0 {
		size = attach.Size{Rows: uint16(rows), Cols: uint16(cols)}
	}
	// Signals are caught before raw mode, so one arriving at any point
	// after it still restores the terminal.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

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
	var once sync.Once
	restore := func() { once.Do(func() { _ = term.Restore(in, old) }) }
	defer restore() // also runs while a panic in this goroutine unwinds
	// guard restores the terminal before a panic in another goroutine
	// kills the process (deferred calls here would not run).
	guard := func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
	}

	events := make(chan attachEvent, 3)
	go func() { // server → terminal
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
			if _, err := stdout.Write(m.Data); err != nil {
				events <- attachEvent{err: fmt.Errorf("write to the terminal: %w", err)}
				return
			}
		}
	}()
	go func() { // terminal → server
		defer guard()
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			p := buf[:n]
			if key != noDetachKey {
				if i := strings.IndexByte(string(p), byte(key)); i >= 0 {
					if i > 0 {
						_ = c.Send(p[:i])
					}
					events <- attachEvent{detach: true}
					return
				}
			}
			if len(p) > 0 {
				if serr := c.Send(p); serr != nil {
					events <- attachEvent{err: serr}
					return
				}
			}
			if err != nil {
				events <- attachEvent{detach: true} // the terminal went away
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

	select {
	case sig := <-sigs:
		_ = c.Detach()
		restore()
		fmt.Fprintf(stderr, "\r\n[fastener: %v; detached from %s]\n", sig, name)
		return 128 + int(sig.(syscall.Signal))
	case ev := <-events:
		switch {
		case ev.detach:
			_ = c.Detach()
			restore()
			fmt.Fprintf(stderr, "\r\n[fastener: detached from %s]\n", name)
			return exitOK
		case ev.exit != nil:
			restore()
			st := session.Status{EndedBy: ev.exit.EndedBy, ExitCode: ev.exit.ExitCode, Signal: ev.exit.Signal, Error: ev.exit.Error}
			fmt.Fprintf(stderr, "\r\n[fastener: session %s %s]\n", name, describeExit(st))
			return exitCodeOf(st)
		default:
			restore()
			var re *attach.RemoteError
			if errors.As(ev.err, &re) {
				fmt.Fprintf(stderr, "\r\n[fastener: the monitor disconnected us from %s: %s]\n", name, re.Message)
			} else {
				fmt.Fprintf(stderr, "\r\n[fastener: lost the connection to %s: %v]\n", name, ev.err)
			}
			return exitAbnormal
		}
	}
}

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
