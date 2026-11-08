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
}

// Load reads a session. handshake is the configured handshake timeout; a
// starting session with no monitor record is lost once older than 2×handshake.
func Load(name string, handshake time.Duration) (*View, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	d := DirFor(name)
	if d == "" {
		return nil, ErrNoStateDir
	}
	if _, err := os.Stat(string(d)); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	v := &View{Dir: d}
	if err := ReadJSON(d.SessionPath(), &v.Session); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := ReadJSON(d.StatusPath(), &v.Status); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	gen := v.Generation()
	var p Plan
	if ReadJSON(d.PlanPath(gen), &p) == nil {
		v.Plan = &p
	}
	var l LauncherRecord
	if ReadJSON(d.LauncherPath(gen), &l) == nil {
		v.Launcher = &l
	}
	v.resolve(handshake, time.Now())
	return v, nil
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

func (v *View) resolve(handshake time.Duration, now time.Time) {
	v.State = v.Status.State
	if v.State == StateRunning || v.State == StateStarting {
		id, boot, ok := v.MonitorIdentity()
		switch {
		case ok && proc.MaybeAlive(id, boot):
			// still supervised (or liveness unknown: never declare lost on a read error)
		case !ok && v.State == StateStarting && now.Sub(v.Session.CreatedAt) < 2*handshake:
			// launcher record not written yet; give it time
		default:
			v.State = StateLost
		}
	}
	if v.State == StateLost && v.Plan != nil {
		v.Orphans, _ = proc.FindMarked(v.Status.PGID, v.Plan.Marker, v.Status.BootID)
	}
}
