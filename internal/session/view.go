package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

// View is a session as read from disk, with its effective state resolved
// against live process state.
type View struct {
	Dir      Dir
	Session  Session
	Status   Status
	Plan     *Plan
	Launcher *LauncherRecord
	State    string          // effective: may be "lost" although status.json says otherwise
	Orphans  []proc.Identity // marked processes still alive in a lost session
	// Warnings are non-fatal read problems, e.g. a file written by a newer
	// fastener than this one.
	Warnings []string
}

// warnNewer records a warning when a file's schema_version is newer than this
// build understands.
func (v *View) warnNewer(file string, version int) {
	if version > SchemaVersion {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"%s has schema_version %d; this fastener understands up to %d, so some fields may be ignored",
			file, version, SchemaVersion))
	}
}

// Load reads a session. handshake is the configured handshake timeout; a
// starting session with no monitor record is lost once older than 2×handshake.
// A missing status.json reads as "starting"; a missing session.json is
// replaced by a stub named after the directory. Corrupt files are errors.
// Load does not scan for orphans; use LoadFull for that.
func Load(name string, handshake time.Duration) (*View, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	d := DirFor(name)
	if d == "" {
		return nil, ErrNoStateDir
	}
	fi, err := os.Stat(string(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	v := &View{Dir: d}
	if err := ReadJSON(d.SessionPath(), &v.Session); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		v.Session = Session{Name: name, CreatedAt: fi.ModTime()}
	}
	v.warnNewer("session.json", v.Session.SchemaVersion)
	if err := ReadJSON(d.StatusPath(), &v.Status); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		v.Status = Status{State: StateStarting}
	}
	v.warnNewer("status.json", v.Status.SchemaVersion)
	gen := v.Generation()
	var p Plan
	if ReadJSON(d.PlanPath(gen), &p) == nil {
		v.Plan = &p
		v.warnNewer(fmt.Sprintf("generations/%d.json", gen), p.SchemaVersion)
	}
	v.Launcher = readLauncher(d, gen)
	if v.Launcher != nil {
		v.warnNewer(fmt.Sprintf("generations/%d.launcher.json", gen), v.Launcher.SchemaVersion)
	}
	v.resolve(handshake, time.Now(), func() (Status, *LauncherRecord, *Plan, error) {
		var st Status
		if err := ReadJSON(d.StatusPath(), &st); err != nil {
			return Status{}, nil, nil, err
		}
		g := 1
		if st.Generation > 0 {
			g = st.Generation
		}
		var np *Plan
		if v.Plan == nil || v.Plan.Generation != g {
			var p Plan
			if ReadJSON(d.PlanPath(g), &p) == nil {
				np = &p
			}
		}
		return st, readLauncher(d, g), np, nil
	})
	return v, nil
}

func readLauncher(d Dir, gen int) *LauncherRecord {
	var l LauncherRecord
	if ReadJSON(d.LauncherPath(gen), &l) == nil {
		return &l
	}
	return nil
}

// LoadFull is Load plus an orphan scan for lost and exited sessions.
func LoadFull(name string, handshake time.Duration) (*View, error) {
	v, err := Load(name, handshake)
	if err != nil {
		return nil, err
	}
	v.FindOrphans()
	return v, nil
}

// FindOrphans fills Orphans with marked processes still alive in a lost
// session, or in an exited session whose agent left descendants behind (they
// are tracked, never killed implicitly). It is a no-op for other states, and
// for exited sessions that never spawned an agent.
func (v *View) FindOrphans() {
	scan := v.State == StateLost || (v.State == StateExited && v.Status.PGID > 0)
	if scan && v.Plan != nil {
		v.Orphans, _ = proc.FindMarked(v.Status.PGID, v.Plan.Marker, v.Status.BootID)
	}
}

// Generation is the current generation (1 if status predates the field).
func (v *View) Generation() int {
	if v.Status.Generation > 0 {
		return v.Status.Generation
	}
	return 1
}

// MonitorIdentity returns the best-known monitor identity: from status.json,
// else from the launcher record.
func (v *View) MonitorIdentity() (proc.Identity, string, bool) {
	if v.Status.Monitor != nil {
		return *v.Status.Monitor, v.Status.BootID, true
	}
	if v.Launcher != nil && v.Launcher.Monitor != nil {
		return *v.Launcher.Monitor, v.Launcher.BootID, true
	}
	return proc.Identity{}, "", false
}

// Alive reports whether the session is still being supervised.
func (v *View) Alive() bool { return v.State == StateRunning || v.State == StateStarting }

// resolve computes the effective state. reload re-reads status.json and the
// current launcher record; it is consulted once before declaring a session
// lost, because the monitor always writes before it exits.
func (v *View) resolve(handshake time.Duration, now time.Time, reload func() (Status, *LauncherRecord, *Plan, error)) {
	v.State = v.Status.State
	if v.State != StateRunning && v.State != StateStarting {
		return
	}
	// TODO(v0.2): a systemd launcher record with only Unit must check the unit's state.
	id, boot, ok := v.MonitorIdentity()
	switch {
	case ok && proc.MaybeAlive(id, boot):
		return // still supervised (or liveness unknown: never declare lost on a read error)
	case !ok && v.State == StateStarting && now.Sub(v.Session.CreatedAt) < 2*handshake:
		return // launcher record not written yet; give it time
	}
	if reload != nil {
		if st, l, p, err := reload(); err == nil {
			v.Status, v.Launcher = st, l
			if p != nil {
				v.Plan = p // the generation changed under us
			}
			if st.State != StateRunning && st.State != StateStarting {
				v.State = st.State // terminal or unknown future state: use as is
				return
			}
			if id2, boot2, ok2 := v.MonitorIdentity(); ok2 && (!ok || id2 != id) && proc.MaybeAlive(id2, boot2) {
				v.State = st.State
				return
			}
		}
	}
	v.State = StateLost
}
