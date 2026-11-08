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
		switch v.State {
		case session.StateExited:
			return exitCodeOf(v.Status)
		case session.StateLost, session.StateRestoreFailed:
			fmt.Fprintf(stderr, "fastener: session %s is %s\n", name, v.State)
			return exitAbnormal
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
		return *s.ExitCode
	}
	return exitAbnormal
}
