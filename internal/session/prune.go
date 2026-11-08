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

// PruneDecision applies the retention rules to a loaded view. Only exited and
// restore-failed sessions are ever removed. Lost sessions are never removed
// (they may hold the only record of live processes), and neither are
// pinned sessions (restart=on-reboot) or ones with orphans. The caller must
// have run FindOrphans for the orphan rule to mean anything.
//
// Age is ended_at, else created_at (a session that never recorded an end).
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
	}
	return PruneRemove, ""
}

// pruneOld reports whether the session ended (or, lacking that, was created)
// more than retention ago.
func (v *View) pruneOld(now time.Time, retention time.Duration) bool {
	t := v.Session.CreatedAt
	if v.Status.EndedAt != nil {
		t = *v.Status.EndedAt
	}
	return now.Sub(t) > retention
}

// PruneOptions configures Prune and AutoPrune.
type PruneOptions struct {
	Retention time.Duration
	Handshake time.Duration
	DryRun    bool
	Now       time.Time    // zero means time.Now()
	Warn      func(string) // optional: unreadable sessions and the like
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

// Prune removes every session PruneDecision approves. Each candidate is
// re-read with an orphan scan immediately before deletion, and a session that
// is alive or has orphans at that point is kept, exactly as rm refuses it.
func Prune(o PruneOptions) PruneResult {
	res := PruneResult{Removed: []string{}, Kept: []PruneKept{}}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	warn := func(s string) {
		if o.Warn != nil {
			o.Warn(s)
		}
	}
	names, err := Names()
	if err != nil {
		warn(err.Error())
		return res
	}
	for _, n := range names {
		v, err := Load(n, o.Handshake)
		if err != nil {
			if !errors.Is(err, ErrNotFound) { // vanished: a concurrent prune or rm
				warn(fmt.Sprintf("skipping %s: %v", n, err))
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
			if !o.DryRun {
				if err := os.RemoveAll(string(v.Dir)); err != nil {
					res.Failed = append(res.Failed, PruneFailure{n, err.Error()})
					continue
				}
			}
			res.Removed = append(res.Removed, n)
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
// to take the lock) is never fresh.
func stampFresh(fi os.FileInfo, now time.Time) bool {
	return fi.Size() > 0 && now.Sub(fi.ModTime()) < PruneInterval
}

// AutoPrune is the opportunistic prune run by `run` and `ps`. It does nothing
// (one stat) if the stamp is under an hour old, skips if another process
// holds the stamp's lock, and ignores every error: it must never slow down or
// fail the command that called it.
func AutoPrune(o PruneOptions) {
	stamp := StampPath()
	if stamp == "" {
		return
	}
	now := time.Now()
	if fi, err := os.Stat(stamp); err == nil && stampFresh(fi, now) {
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
		return // someone else is pruning
	}
	if fi, err := f.Stat(); err == nil && stampFresh(fi, time.Now()) {
		return // finished by someone else between our stat and our lock
	}
	o.DryRun = false
	Prune(o)
	touchStamp(f)
}

// PruneLocked is the on-demand prune: it waits for any other prune to finish
// (so concurrent calls serialize), prunes, and refreshes the stamp unless it
// was a dry run. A failure to take the lock is not fatal.
func PruneLocked(o PruneOptions) PruneResult {
	if stamp := StampPath(); stamp != "" && !o.DryRun {
		if _, err := os.Stat(SessionsDir()); err == nil {
			if f, err := os.OpenFile(stamp, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
				defer f.Close()
				if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) == nil {
					res := Prune(o)
					touchStamp(f)
					return res
				}
			}
		}
	}
	return Prune(o)
}

func touchStamp(f *os.File) {
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0)
}
