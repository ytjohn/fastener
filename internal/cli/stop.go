package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
		return parseFail(err)
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
		return parseFail(err)
	}
	name, ok := oneName(pos, stderr, "rm <name> [--force]")
	if !ok {
		return exitUsage
	}
	v, err := loadView(name)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return fail(stderr, err)
		}
		if !*force {
			return fail(stderr, fmt.Errorf("%w (the session may be corrupt; use --force to remove it)", err))
		}
		if err := cleanCorrupt(name, stderr); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	if v.OrphanErr != "" {
		fmt.Fprintf(stderr, "fastener: warning: %s: orphan check unavailable (%s); leftover processes may not be detected\n", name, v.OrphanErr)
	}
	if v.Alive() || len(v.Orphans) > 0 {
		if !*force {
			return fail(stderr, fmt.Errorf("session %s is %s (orphans: %d); stop it first or use --force", name, v.State, len(v.Orphans)))
		}
		if err := stopSession(v, 0, "rm"); err != nil {
			return fail(stderr, err)
		}
	}
	// Claim through a tombstone, so a session recreated under this name
	// while we were looking is never deleted.
	removed, err := session.Reap(v, handshakeTimeout(), func(nv *session.View) bool {
		return !nv.Alive() && len(nv.Orphans) == 0
	}, func(s string) { fmt.Fprintf(stderr, "fastener: warning: %s\n", s) })
	if err != nil {
		return fail(stderr, err)
	}
	if !removed {
		return fail(stderr, fmt.Errorf("session %s changed while being removed; nothing was deleted", name))
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
	case v.State == session.StateExited && len(v.Orphans) > 0:
		// The agent ended normally but left descendants. status.json is already
		// final, so just clean them up; do not re-finalize.
		if left := killOrphans(v.Orphans, v.Status.BootID, grace); len(left) > 0 {
			return fmt.Errorf("session %s: %d orphaned process(es) survived SIGKILL (%s)", v.Session.Name, len(left), pidList(left))
		}
		return nil
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
			if left := killOrphans(v.Orphans, v.Status.BootID, grace); len(left) > 0 {
				return fmt.Errorf("session %s: %d orphaned process(es) survived SIGKILL (%s); not finalizing", v.Session.Name, len(left), pidList(left))
			}
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

// killOrphans signals each identified pid (never the pgid as a whole) and
// returns the ones that may still be alive afterwards.
func killOrphans(ids []proc.Identity, boot string, grace time.Duration) []proc.Identity {
	for _, id := range ids {
		_ = proc.Signal(id, boot, syscall.SIGTERM)
	}
	left := proc.WaitGone(ids, boot, grace)
	for _, id := range left {
		_ = proc.Signal(id, boot, syscall.SIGKILL)
	}
	return proc.WaitGone(left, boot, 2*time.Second)
}

func pidList(ids []proc.Identity) string {
	var parts []string
	for _, id := range ids {
		parts = append(parts, strconv.Itoa(id.PID))
	}
	return strings.Join(parts, ", ")
}

// cleanCorrupt is rm --force for a session that cannot be loaded: kill any
// marked processes it can still identify from whatever parses, then delete.
func cleanCorrupt(name string, stderr io.Writer) error {
	d := session.DirFor(name)
	if d == "" {
		return session.ErrNoStateDir
	}
	var st session.Status
	if err := session.ReadJSON(d.StatusPath(), &st); err == nil && st.PGID > 0 {
		gen := st.Generation
		if gen <= 0 {
			gen = 1
		}
		var p session.Plan
		if perr := session.ReadJSON(d.PlanPath(gen), &p); perr != nil {
			fmt.Fprintf(stderr, "fastener: warning: %s: cannot read plan for orphan cleanup: %v\n", name, perr)
		} else if p.Marker != "" {
			if ids, _ := proc.FindMarked(st.PGID, p.Marker, st.BootID); len(ids) > 0 {
				if left := killOrphans(ids, st.BootID, 10*time.Second); len(left) > 0 {
					return fmt.Errorf("session %s: %d orphaned process(es) survived SIGKILL (%s); not removing", name, len(left), pidList(left))
				}
			}
		}
	} else if err != nil {
		fmt.Fprintf(stderr, "fastener: warning: %s: cannot read status for orphan cleanup: %v\n", name, err)
	}
	return os.RemoveAll(string(d))
}
