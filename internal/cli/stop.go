package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
)

func cmdStop(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop <name>", stderr)
	grace := fs.Duration("grace", 0, "time between SIGTERM and SIGKILL (default: the session's)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	name, ok := oneName(pos, stderr, "stop <name> [--grace D]")
	if !ok {
		return exitUsage
	}
	v, err := loadView(name)
	if err != nil {
		return fail(stderr, err)
	}
	if err := stopSession(v, *grace, "stop"); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func cmdRm(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rm <name>", stderr)
	force := fs.Bool("force", false, "stop the session first if it is alive or has orphans")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	name, ok := oneName(pos, stderr, "rm <name> [--force]")
	if !ok {
		return exitUsage
	}
	v, err := loadView(name)
	if err != nil {
		return fail(stderr, err)
	}
	if v.Alive() || len(v.Orphans) > 0 {
		if !*force {
			return fail(stderr, fmt.Errorf("session %s is %s (orphans: %d); stop it first or use --force", name, v.State, len(v.Orphans)))
		}
		if err := stopSession(v, 0, "rm"); err != nil {
			return fail(stderr, err)
		}
	}
	if err := os.RemoveAll(string(v.Dir)); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// stopSession stops a session through its monitor, or, for a lost session,
// signals each verified orphan and finalizes status.json.
func stopSession(v *session.View, grace time.Duration, by string) error {
	return stopOnce(v, grace, by, false)
}

// stopOnce does the work; retried is true once a lost session has turned out
// to have a live monitor, so that path cannot recurse forever.
func stopOnce(v *session.View, grace time.Duration, by string, retried bool) error {
	if grace <= 0 {
		grace = time.Duration(v.Session.Grace)
	}
	if grace <= 0 {
		grace = 10 * time.Second
	}
	switch {
	case v.State == session.StateExited || v.State == session.StateRestoreFailed:
		return nil
	case v.Alive():
		req := session.StopRequest{
			SchemaVersion: session.SchemaVersion, Generation: v.Generation(),
			RequestedAt: time.Now().UTC(), Grace: session.Duration(grace), By: by,
		}
		if err := session.WriteJSON(v.Dir.StopPath(), req); err != nil {
			return err
		}
		if id, boot, ok := v.MonitorIdentity(); ok {
			if err := proc.Signal(id, boot, syscall.SIGTERM); err != nil && !errors.Is(err, proc.ErrNoProcess) {
				return fmt.Errorf("signal monitor of %s: %w", v.Session.Name, err)
			}
		} // else: the monitor has not started yet and will honour the request on start
		return waitStopped(v.Session.Name, grace+2*handshakeTimeout()+10*time.Second, grace, by)
	case v.State == session.StateLost:
		if len(v.Orphans) > 0 {
			killOrphans(v.Orphans, v.Status.BootID, grace)
		}
		_, err := session.Finalize(v.Dir, func(s *session.Status) {
			now := time.Now().UTC()
			s.State, s.EndedBy, s.EndedAt = session.StateExited, session.EndedByStop, &now
			if s.Error == "" {
				s.Error = "monitor was lost; finalized by " + by
			}
		})
		if errors.Is(err, session.ErrMonitorAlive) && !retried {
			// The monitor is (maybe) back: take the normal path, once.
			nv, lerr := loadView(v.Session.Name)
			if lerr != nil {
				return lerr
			}
			return stopOnce(nv, grace, by, true)
		}
		return err
	}
	return fmt.Errorf("session %s in unexpected state %q", v.Session.Name, v.State)
}

func waitStopped(name string, timeout, grace time.Duration, by string) error {
	deadline := time.Now().Add(timeout)
	for {
		v, err := loadView(name)
		if err != nil {
			return err
		}
		switch v.State {
		case session.StateExited:
			return nil
		case session.StateLost: // the monitor died mid-stop: clean up directly
			return stopOnce(v, grace, by, true)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s did not stop within %s; see %s", name, timeout, v.Dir.MonitorLog())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// killOrphans signals each identified pid (never the pgid as a whole).
func killOrphans(ids []proc.Identity, boot string, grace time.Duration) {
	for _, id := range ids {
		_ = proc.Signal(id, boot, syscall.SIGTERM)
	}
	left := proc.WaitGone(ids, boot, grace)
	for _, id := range left {
		_ = proc.Signal(id, boot, syscall.SIGKILL)
	}
	proc.WaitGone(left, boot, 2*time.Second)
}
