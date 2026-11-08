package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// PruneVerdict is the retention decision for one session.
type PruneVerdict int

const (
	PruneSkip   PruneVerdict = iota // not a candidate (alive, or not old enough); say nothing
	PruneRemove                     // ended, old, unpinned, no orphans
	PruneKeep                       // old enough, but must be kept; the reason says why
)

// PruneInterval is the minimum time between opportunistic prunes.
const PruneInterval = time.Hour

// MinRetention is the smallest retention the config accepts.
const MinRetention = time.Hour

// ErrPruneBusy is returned by PruneLocked when another prune holds the lock.
var ErrPruneBusy = errors.New("another prune is running")

// lockWait bounds how long an on-demand prune waits for another prune. A var
// so tests can shorten it.
var lockWait = 10 * time.Second

// beforeDelete is a test hook called after a session is judged prunable and
// before it is claimed for deletion.
var beforeDelete func(name string)

// PruneDecision applies the retention rules to a loaded view. Only exited and
// restore-failed sessions are ever removed. Lost sessions are never removed
// (they may hold the only record of live processes), and neither are pinned
// sessions (restart=on-reboot), ones with orphans, or ones whose orphan state
// is unknown (fail closed). The caller must have run FindOrphans.
func (v *View) PruneDecision(now time.Time, retention time.Duration) (PruneVerdict, string) {
	switch v.State {
	case StateExited, StateRestoreFailed, StateLost:
	default:
		return PruneSkip, ""
	}
	if !v.pruneOld(now, retention) {
		return PruneSkip, ""
	}
	switch {
	case v.State == StateLost:
		return PruneKeep, "lost"
	case v.Session.Restart == "on-reboot":
		return PruneKeep, "pinned"
	case len(v.Orphans) > 0:
		return PruneKeep, "orphans"
	case v.OrphanErr != "":
		return PruneKeep, "unknown" // orphan scan unavailable
	}
	return PruneRemove, ""
}

// pruneOld reports whether the session's last recorded time, the later of
// ended_at and created_at (zero values ignored), is more than retention ago.
// With no usable timestamp the session is never old.
func (v *View) pruneOld(now time.Time, retention time.Duration) bool {
	var t time.Time
	if !v.Session.CreatedAt.IsZero() {
		t = v.Session.CreatedAt
	}
	if e := v.Status.EndedAt; e != nil && !e.IsZero() && e.After(t) {
		t = *e
	}
	return !t.IsZero() && now.Sub(t) > retention
}

// sameSession reports whether b is the session a was read as: the same
// creation time, generation and process marker. A session replaced under the
// same name differs in at least created_at.
func sameSession(a, b *View) bool {
	marker := func(v *View) string {
		if v.Plan != nil {
			return v.Plan.Marker
		}
		return ""
	}
	return a.Session.CreatedAt.Equal(b.Session.CreatedAt) &&
		a.Generation() == b.Generation() && marker(a) == marker(b)
}

// PruneOptions configures Prune and AutoPrune.
type PruneOptions struct {
	Retention time.Duration
	Handshake time.Duration
	DryRun    bool
	Now       time.Time    // zero means time.Now()
	Warn      func(string) // optional: unreadable sessions, leftover tombstones, ...
}

func (o PruneOptions) warn(s string) {
	if o.Warn != nil {
		o.Warn(s)
	}
}

// PruneKept is a session that was old enough to prune but was kept.
type PruneKept struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// PruneFailure is a removal that failed.
type PruneFailure struct {
	Name string `json:"name"`
	Err  string `json:"error"`
}

// PruneResult lists what was (or, for a dry run, would be) removed.
type PruneResult struct {
	Removed []string
	Kept    []PruneKept
	Failed  []PruneFailure
}

// Reap deletes the session v was read as, safely against the name being
// reused while v was being judged. It renames the directory to a hidden
// tombstone (ValidName rejects the leading dot, so no command sees it),
// re-loads it from there, and deletes it only if it is still the same session
// and ok accepts the fresh view (which has had its orphan scan). Otherwise it
// renames it back; if the name has been taken meanwhile it leaves the
// tombstone and warns. removed is false with a nil error when the session was
// put back or had already vanished.
func Reap(v *View, handshake time.Duration, ok func(*View) bool, warn func(string)) (removed bool, err error) {
	name := v.Session.Name
	if name == "" {
		name = filepath.Base(string(v.Dir))
	}
	orig := string(v.Dir)
	tomb := filepath.Join(filepath.Dir(orig), ".prune-"+name+"-"+randToken(5))
	if err := os.Rename(orig, tomb); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	nv, lerr := LoadAt(Dir(tomb), name, handshake)
	if lerr == nil {
		nv.FindOrphans()
		if sameSession(v, nv) && ok(nv) {
			return true, os.RemoveAll(tomb)
		}
	}
	if _, serr := os.Lstat(orig); serr == nil {
		warn(fmt.Sprintf("%s was replaced while being removed; the old copy is left at %s", name, tomb))
		return false, nil
	}
	if err := os.Rename(tomb, orig); err != nil {
		warn(fmt.Sprintf("could not restore %s; it is left at %s: %v", name, tomb, err))
	}
	return false, nil
}

// Prune removes every session PruneDecision approves, claiming each through
// Reap so a session replaced under the same name is never deleted.
func Prune(o PruneOptions) PruneResult {
	res := PruneResult{Removed: []string{}, Kept: []PruneKept{}}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	names, err := Names()
	if err != nil {
		o.warn(err.Error())
		return res
	}
	for _, n := range names {
		v, err := Load(n, o.Handshake)
		if err != nil {
			if !errors.Is(err, ErrNotFound) { // vanished: a concurrent prune or rm
				o.warn(fmt.Sprintf("skipping %s: %v", n, err))
			}
			continue
		}
		if v.pruneOld(now, o.Retention) {
			v.FindOrphans()
		}
		verdict, why := v.PruneDecision(now, o.Retention)
		switch verdict {
		case PruneKeep:
			res.Kept = append(res.Kept, PruneKept{n, why})
		case PruneRemove:
			if o.DryRun {
				res.Removed = append(res.Removed, n)
				continue
			}
			if beforeDelete != nil {
				beforeDelete(n)
			}
			removed, err := Reap(v, o.Handshake, func(nv *View) bool {
				d, _ := nv.PruneDecision(now, o.Retention)
				return d == PruneRemove
			}, o.warn)
			switch {
			case err != nil:
				res.Failed = append(res.Failed, PruneFailure{n, err.Error()})
			case removed:
				res.Removed = append(res.Removed, n)
			}
		}
	}
	return res
}

// StampPath is the prune throttle stamp.
func StampPath() string {
	r := Root()
	if r == "" {
		return ""
	}
	return filepath.Join(r, ".last-prune")
}

// stampFresh: a completed prune writes a byte, so an empty file (just created
// to take the lock) is never fresh. A stamp from the future is stale.
func stampFresh(fi os.FileInfo, now time.Time) bool {
	age := now.Sub(fi.ModTime())
	return fi.Size() > 0 && age >= 0 && age < PruneInterval
}

// AutoPrune is the opportunistic prune run by `run` and `ps`. It does nothing
// (one stat) if the stamp is fresh, skips if another process holds the
// stamp's lock, and ignores every error: it must never slow down or fail the
// command that called it.
func AutoPrune(o PruneOptions) {
	stamp := StampPath()
	if stamp == "" {
		return
	}
	if fi, err := os.Stat(stamp); err == nil && stampFresh(fi, time.Now()) {
		return
	}
	if _, err := os.Stat(SessionsDir()); err != nil {
		return // nothing to prune; do not create the state root
	}
	f, err := os.OpenFile(stamp, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return // someone else is pruning (or locking is unsupported)
	}
	if fi, err := f.Stat(); err == nil && stampFresh(fi, time.Now()) {
		return // finished by someone else between our stat and our lock
	}
	o.DryRun = false
	o.Warn = nil
	Prune(o)
	touchStamp(f)
}

// PruneLocked is the on-demand prune. It polls the stamp's lock for up to 10s
// and returns ErrPruneBusy if another prune still holds it. If the filesystem
// does not support flock it proceeds unlocked, with a warning. A dry run
// takes no lock.
func PruneLocked(o PruneOptions) (PruneResult, error) {
	stamp := StampPath()
	if stamp == "" || o.DryRun {
		return Prune(o), nil
	}
	if _, err := os.Stat(SessionsDir()); err != nil {
		return Prune(o), nil
	}
	f, err := os.OpenFile(stamp, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		o.warn(fmt.Sprintf("cannot open %s, pruning without a lock: %v", stamp, err))
		return Prune(o), nil
	}
	defer f.Close()
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.EOPNOTSUPP) {
			o.warn("file locking is not supported here; pruning without a lock")
			return Prune(o), nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return PruneResult{}, err
		}
		if time.Now().After(deadline) {
			return PruneResult{}, ErrPruneBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
	res := Prune(o)
	touchStamp(f)
	return res, nil
}

func touchStamp(f *os.File) {
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0)
}
