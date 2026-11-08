package cli

import (
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ytjohn/fastener/internal/session"
)

func cmdWait(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("wait <name>", stderr)
	timeout := fs.Duration("timeout", 0, "give up after this long (exit 124)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	name, ok := oneName(pos, stderr, "wait <name> [--timeout D]")
	if !ok {
		return exitUsage
	}
	var deadline time.Time
	if *timeout > 0 {
		deadline = time.Now().Add(*timeout)
	}
	hs := handshakeTimeout()
	for {
		v, err := session.LoadFull(name, hs)
		if err != nil {
			return fail(stderr, err)
		}
		if v.State == session.StateExited || v.State == session.StateLost || v.State == session.StateRestoreFailed {
			return outcomeCode(name, v, stderr)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return exitWaitTimeout
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// exitCodeOf maps an exited status to wait's exit code (spec §10).
func exitCodeOf(s session.Status) int {
	if s.EndedBy == session.EndedByLaunchFailed {
		return exitAbnormal
	}
	if s.Signal != "" {
		if n := int(unix.SignalNum(s.Signal)); n > 0 {
			return 128 + n
		}
		return exitAbnormal
	}
	if s.ExitCode != nil {
		// Some agents (pi's json mode) exit 0 after a failed model call; only
		// the parser's agent_error tells. Report that as abnormal, not success.
		if *s.ExitCode == 0 && s.AgentError != nil && *s.AgentError {
			return exitAbnormal
		}
		return *s.ExitCode
	}
	return exitAbnormal
}

// outcomeCode is the exit code `wait` (and `run --follow`) reports for a
// session that is no longer alive, with the matching diagnostic on stderr.
func outcomeCode(name string, v *session.View, stderr io.Writer) int {
	switch v.State {
	case session.StateExited:
		code := exitCodeOf(v.Status)
		if code == exitAbnormal && v.Status.ExitCode != nil && *v.Status.ExitCode == 0 {
			fmt.Fprintf(stderr, "fastener: session %s: agent reported an error (exit code 0)\n", name)
		}
		return code
	default:
		fmt.Fprintf(stderr, "fastener: session %s is %s\n", name, v.State)
		return exitAbnormal
	}
}
