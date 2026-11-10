package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/parser"
	"github.com/ytjohn/fastener/internal/session"
)

// Reasons `run --remote --json` reports when a bridge ends before it has
// reported its URL.
const (
	reasonUntrusted = "untrusted_workspace" // the folder is not trusted by the backend
	reasonExited    = "exited"              // any other end before run saw the URL
)

// remotePoll is how often `run --remote` re-reads the session while waiting
// for the URL. The monitor flushes parser fields about once a second.
const remotePoll = 100 * time.Millisecond

// stderrTail caps how much of stderr.log is read to explain an early end.
const stderrTail = 64 << 10

// remoteFailure is a bridge that ended before reporting its URL.
type remoteFailure struct {
	reason, folder, msg string
}

func (e *remoteFailure) Error() string { return e.msg }

// waitRemoteURL polls the session after the handshake until the bridge
// reports remote_url, the session ends, or wait elapses. It returns the last
// view read. A session that ended (even one that printed a URL first) is a
// *remoteFailure; a timeout while the bridge still runs is not an error, and
// the view then has no remote_url.
//
// An interactive remote session does not exit in an untrusted folder: its
// TUI shows the trust dialog and waits. So while it runs, its terminal
// output is checked against untrusted_pattern too; on a match the session
// is stopped and reported untrusted, exactly as a bridge that exited.
func waitRemoteURL(v *session.View, p *prepared, wait time.Duration) (*session.View, error) {
	name := v.Session.Name
	deadline := time.Now().Add(wait)
	for {
		if !v.Alive() {
			full := withOrphanScan(v, name, p.cfg.Handshake(), session.LoadFull)
			return full, explainEarlyEnd(full, p)
		}
		if p.interactive && v.Status.RemoteURL == "" {
			if f := untrustedFailure(v, p); f != nil {
				outcome := "the session was stopped"
				if err := stopSession(v, 0, "stop"); err != nil {
					outcome = "stopping it failed: " + err.Error()
				}
				f.msg += " (" + outcome + ")"
				if nv, err := session.LoadFull(name, p.cfg.Handshake()); err == nil {
					v = nv
				}
				return v, f
			}
		}
		if v.Status.RemoteURL != "" || !time.Now().Before(deadline) {
			return v, nil
		}
		time.Sleep(remotePoll)
		nv, err := session.Load(name, p.cfg.Handshake())
		if err != nil {
			return v, err
		}
		v = nv
	}
}

// explainEarlyEnd builds the failure for a bridge that ended: an untrusted
// folder when stderr.log matches the profile's untrusted_pattern (with the
// profile's untrusted_hint, if any), else the way it ended, the URL if it
// reported one, and the last line of stderr.
func explainEarlyEnd(v *session.View, p *prepared) error {
	if f := untrustedFailure(v, p); f != nil {
		return f
	}
	tail := readTail(v.Dir.StderrLog(), stderrTail)
	if p.interactive { // a pty merges stderr into the terminal output
		tail = parser.StripForMatch(readTail(v.Dir.OutputLog(), stderrTail))
	}
	how := []string{"state=" + v.State}
	if s := v.Status; s.EndedBy != "" {
		how = append(how, "ended_by="+s.EndedBy)
		if s.ExitCode != nil {
			how = append(how, fmt.Sprintf("exit_code=%d", *s.ExitCode))
		}
		if s.Signal != "" {
			how = append(how, "signal="+s.Signal)
		}
	}
	what := "before reporting its URL"
	if u := v.Status.RemoteURL; u != "" {
		what = "after reporting " + u // it started, then went away before run saw the URL
	}
	msg := fmt.Sprintf("session %s: the %s ended %s (%s)", v.Session.Name, remoteNoun(p), what, strings.Join(how, ", "))
	if v.Status.Error != "" {
		msg += ": " + v.Status.Error
	}
	if line := lastLine(tail); line != "" {
		if p.interactive {
			msg += "; terminal: " + line
		} else {
			msg += "; stderr: " + line
		}
	}
	return &remoteFailure{reason: reasonExited, msg: msg}
}

// trustCheck is the untrusted pattern and hint for this kind of remote run,
// and the output they are matched against: stderr.log for a bridge, the
// terminal output (escapes stripped) for an interactive session.
func trustCheck(v *session.View, p *prepared) (pattern, hint string, text []byte) {
	if p.interactive {
		if in := p.prof.Interactive; in != nil && in.Remote != nil {
			pattern, hint = in.Remote.UntrustedPattern, in.Remote.UntrustedHint
		}
		if pattern != "" {
			text = withoutPrompt(parser.StripForMatch(readTail(v.Dir.OutputLog(), stderrTail)), v.Dir.PromptPath())
		}
		return
	}
	if r := p.prof.Remote; r != nil {
		pattern, hint = r.UntrustedPattern, r.UntrustedHint
	}
	if pattern != "" {
		text = readTail(v.Dir.StderrLog(), stderrTail)
	}
	return
}

// withoutPrompt removes every echo of the session's initial prompt from
// text, comparing with runs of whitespace collapsed (the TUI may wrap it),
// so a prompt that quotes the trust dialog cannot pass for it. The
// untrusted pattern is the real guard: it matches the dialog's heading and
// its option text together.
func withoutPrompt(text []byte, promptPath string) []byte {
	b, err := os.ReadFile(promptPath)
	if err != nil {
		return text
	}
	prompt := strings.Join(strings.Fields(string(parser.StripForMatch(b))), " ")
	norm := strings.Join(strings.Fields(string(text)), " ")
	if prompt == "" {
		return []byte(norm)
	}
	return []byte(strings.ReplaceAll(norm, prompt, " "))
}

// untrustedFailure reports the folder untrusted when the output matches the
// profile's untrusted_pattern, with its untrusted_hint if any; else nil.
func untrustedFailure(v *session.View, p *prepared) *remoteFailure {
	pattern, hint, text := trustCheck(v, p)
	if pattern == "" || !regexp.MustCompile(pattern).Match(text) {
		return nil
	}
	cwd := v.Session.Cwd
	msg := fmt.Sprintf("%s is not a trusted workspace for %s", cwd, bridgeCommand(p))
	if hint != "" {
		msg += "; authorize it with: " + strings.ReplaceAll(hint, "{dir}", cwd)
	}
	return &remoteFailure{reason: reasonUntrusted, folder: cwd, msg: msg}
}

// remoteNoun names what run --remote started.
func remoteNoun(p *prepared) string {
	if p.interactive {
		return "interactive session"
	}
	return "bridge"
}

// bridgeCommand names the remote command as a user would type it, e.g.
// "claude remote-control" or "claude --remote-control": the command's base
// name and the first remote argument.
func bridgeCommand(p *prepared) string {
	cmd := filepath.Base(p.prof.Command)
	var args []string
	if p.interactive {
		if in := p.prof.Interactive; in != nil && in.Remote != nil {
			args = in.Remote.Args
		}
	} else if r := p.prof.Remote; r != nil {
		args = r.Args
	}
	if len(args) > 0 && !strings.Contains(args[0], "{") {
		cmd += " " + args[0]
	}
	return cmd
}

// readTail returns up to n bytes from the end of path ("" if unreadable).
func readTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		_, _ = f.Seek(fi.Size()-n, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return b
}

// lastLine is the last non-empty line of b, trimmed and cut to 300 bytes.
func lastLine(b []byte) string {
	lines := bytes.Split(bytes.TrimRight(b, "\r\n\t "), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(string(lines[i])); l != "" {
			return truncate(l, 300)
		}
	}
	return ""
}
