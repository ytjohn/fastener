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

	"github.com/ytjohn/fastener/internal/session"
)

// Reasons `run --remote --json` reports when a bridge ends before it has
// reported its URL.
const (
	reasonUntrusted = "untrusted_workspace" // the folder is not trusted by the backend
	reasonExited    = "exited"              // any other early end
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
func waitRemoteURL(v *session.View, p *prepared, wait time.Duration) (*session.View, error) {
	name := v.Session.Name
	deadline := time.Now().Add(wait)
	for {
		if !v.Alive() {
			full := withOrphanScan(v, name, p.cfg.Handshake(), session.LoadFull)
			return full, explainEarlyEnd(full, p)
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
// folder when stderr.log matches the profile's untrusted_pattern, else the
// way it ended and the last line of stderr.
func explainEarlyEnd(v *session.View, p *prepared) error {
	tail := readTail(v.Dir.StderrLog(), stderrTail)
	cwd := v.Session.Cwd
	if r := p.prof.Remote; r != nil && r.UntrustedPattern != "" && regexp.MustCompile(r.UntrustedPattern).Match(tail) {
		return &remoteFailure{reason: reasonUntrusted, folder: cwd, msg: fmt.Sprintf(
			"%s is not a trusted workspace for %s; authorize it with: fastener authorize %s-folder-trust %s",
			cwd, bridgeCommand(p), p.prof.Name, cwd)}
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
	msg := fmt.Sprintf("session %s: the bridge ended before reporting its URL (%s)", v.Session.Name, strings.Join(how, ", "))
	if v.Status.Error != "" {
		msg += ": " + v.Status.Error
	}
	if line := lastLine(tail); line != "" {
		msg += "; stderr: " + line
	}
	return &remoteFailure{reason: reasonExited, msg: msg}
}

// bridgeCommand names the bridge as a user would type it, e.g.
// "claude remote-control": the command's base name and the first argument.
func bridgeCommand(p *prepared) string {
	cmd := filepath.Base(p.prof.Command)
	if r := p.prof.Remote; r != nil && len(r.Args) > 0 && !strings.Contains(r.Args[0], "{") {
		cmd += " " + r.Args[0]
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
