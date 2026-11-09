package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
	"github.com/ytjohn/fastener/internal/systemd"
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
	*grace = clampGrace(v, *grace, stderr)
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
	// Never remove a session a restore is relaunching (it holds restore.lock
	// for the whole relaunch). The lock is only tried, never waited for.
	if unlock, err := session.LockRestore(v.Dir, 0); errors.Is(err, session.ErrRestoreBusy) {
		return fail(stderr, fmt.Errorf("session %s: a restore is in progress; try again once it has finished", name))
	} else if err == nil {
		defer unlock()
	}
	if v.OrphanErr != "" {
		if !*force {
			return fail(stderr, fmt.Errorf("session %s: orphan check unavailable (%s), so leftover processes cannot be ruled out; use --force to remove it anyway", name, v.OrphanErr))
		}
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

// clampGrace limits a per-call --grace to the session's own grace when a live
// session runs under systemd with a max runtime: the unit's RuntimeMaxSec was
// sized from the session's grace, so a longer stop could have that backstop
// fire mid-grace (recorded as signal, not stop). TimeoutStopSec does not bound
// a stop, which the monitor runs itself. It warns when it clamps.
func clampGrace(v *session.View, grace time.Duration, stderr io.Writer) time.Duration {
	if v.Launcher == nil || v.Launcher.Launcher != "systemd" || !v.Alive() || v.Session.MaxRuntime <= 0 {
		return grace
	}
	if limit := v.Session.EffectiveGrace(); grace > limit {
		fmt.Fprintf(stderr, "fastener: warning: session %s runs under systemd with a max runtime; --grace %s is longer than the session's grace and is clamped to %s (the unit's RuntimeMaxSec backstop was sized from it)\n",
			v.Session.Name, grace, limit)
		return limit
	}
	return grace
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
		grace = v.Session.EffectiveGrace()
	}
	switch {
	case (v.State == session.StateExited || v.State == session.StateRestoreFailed) && len(v.Orphans) > 0:
		// The agent ended normally (or a relaunch ended at once) but left
		// descendants. status.json is already final, so just clean them up;
		// do not re-finalize.
		if left := killOrphans(v.Orphans, orphanBoot(v), grace); len(left) > 0 {
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
		} else if unit := unitOnly(v); unit != "" {
			// Only the unit is known: SIGTERM its main process (the monitor).
			// Never `systemctl stop`, which signals the whole cgroup at once.
			if err := systemd.KillMain(unit); err != nil {
				if alive, uerr := systemd.UnitAlive(unit); alive || uerr != nil {
					return fmt.Errorf("signal monitor of %s (unit %s): %w", v.Session.Name, unit, err)
				} // the unit is gone: waitStopped sees the outcome
			}
		} // else: the monitor has not started yet and will honour the request on start
		return waitStopped(v.Session.Name, grace+2*handshakeTimeout()+10*time.Second, grace, by)
	case v.State == session.StateLost:
		if len(v.Orphans) > 0 {
			if left := killOrphans(v.Orphans, orphanBoot(v), grace); len(left) > 0 {
				return fmt.Errorf("session %s: %d orphaned process(es) survived SIGKILL (%s); not finalizing", v.Session.Name, len(left), pidList(left))
			}
		}
		_, err := session.Finalize(v.Dir, func(s *session.Status) {
			now := time.Now().UTC()
			s.State, s.EndedBy, s.EndedAt = session.StateExited, session.EndedByStop, &now
			if s.Error == "" {
				s.Error = lostFinalizedBy + by
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

// lostFinalizedBy starts the error a lost session is finalized with by stop
// or rm ("monitor was lost; finalized by stop").
const lostFinalizedBy = "monitor was lost; finalized by "

// orphanBoot is the boot to verify orphans against: the recorded one, else
// the current boot (an any-group marker scan finds only current-boot
// processes, and a session that never recorded a boot has nothing older).
func orphanBoot(v *session.View) string {
	if b := v.RecordedBoot(); b != "" {
		return b
	}
	b, _ := proc.BootID()
	return b
}

// unitOnly returns the unit of a systemd launcher record with no monitor identity.
func unitOnly(v *session.View) string {
	if v.Launcher == nil || v.Launcher.Monitor != nil {
		return ""
	}
	return v.Launcher.Unit
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
// returns the ones that may still be alive afterwards. Each orphan is opened
// as a proc.Handle up front and both TERM and KILL go through it, so on Linux
// a pid that exits and is reused during the grace period is never signalled.
func killOrphans(ids []proc.Identity, boot string, grace time.Duration) []proc.Identity {
	hs := make(map[proc.Identity]*proc.Handle, len(ids))
	for _, id := range ids {
		if h, err := proc.Open(id, boot); err == nil {
			hs[id] = h
			defer h.Close()
		}
	}
	signal := func(id proc.Identity, sig syscall.Signal) {
		if h := hs[id]; h != nil {
			_ = h.Signal(sig)
			return
		}
		// Not opened: gone already, or liveness was unknown; re-verify now.
		_ = proc.Signal(id, boot, sig)
	}
	for _, id := range ids {
		signal(id, syscall.SIGTERM)
	}
	left := proc.WaitGone(ids, boot, grace)
	for _, id := range left {
		signal(id, syscall.SIGKILL)
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
		// No pgid or boot ID: fall back to a marker-only scan of the current
		// boot, using the markers of whatever plans still parse.
		if err := killByMarkers(name, d, stderr); err != nil {
			return err
		}
	}
	return os.RemoveAll(string(d))
}

// killByMarkers finds and kills processes carrying the marker of any readable
// generation plan of the session, regardless of process group. Identity is
// verified per pid by FindMarkedAnyGroup and killOrphans.
func killByMarkers(name string, d session.Dir, stderr io.Writer) error {
	boot, err := proc.BootID()
	if err != nil {
		fmt.Fprintf(stderr, "fastener: warning: %s: cannot determine boot for orphan cleanup: %v\n", name, err)
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(string(d), "generations", "*.json"))
	var ids []proc.Identity
	readable := false
	for _, pp := range paths {
		var p session.Plan
		if session.ReadJSON(pp, &p) != nil || p.Marker == "" {
			continue
		}
		readable = true
		found, _ := proc.FindMarkedAnyGroup(p.Marker)
		ids = append(ids, found...)
	}
	if !readable {
		fmt.Fprintf(stderr, "fastener: warning: %s: cannot read any plan for orphan cleanup\n", name)
		return nil
	}
	if len(ids) > 0 {
		if left := killOrphans(ids, boot, 10*time.Second); len(left) > 0 {
			return fmt.Errorf("session %s: %d orphaned process(es) survived SIGKILL (%s); not removing", name, len(left), pidList(left))
		}
	}
	return nil
}
