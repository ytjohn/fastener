package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

func TestPruneDecision(t *testing.T) {
	now := time.Now()
	ret := 24 * time.Hour
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	mk := func(state string, ended, created *time.Time, restart string, orphans int) *View {
		v := &View{State: state, Orphans: make([]proc.Identity, orphans)}
		v.Status.EndedAt = ended
		v.Session.Restart = restart
		if created != nil {
			v.Session.CreatedAt = *created
		}
		return v
	}
	cases := []struct {
		name string
		v    *View
		want PruneVerdict
		why  string
	}{
		{"old exited", mk(StateExited, &old, &old, "no", 0), PruneRemove, ""},
		{"old restore-failed", mk(StateRestoreFailed, &old, &old, "", 0), PruneRemove, ""},
		{"recent exited", mk(StateExited, &recent, &old, "", 0), PruneSkip, ""},
		{"exited without ended_at uses created_at", mk(StateExited, nil, &old, "", 0), PruneRemove, ""},
		{"running is never touched", mk(StateRunning, nil, &old, "", 0), PruneSkip, ""},
		{"starting is never touched", mk(StateStarting, nil, &old, "", 0), PruneSkip, ""},
		{"old lost is kept", mk(StateLost, nil, &old, "", 0), PruneKeep, "lost"},
		{"recent lost is silent", mk(StateLost, nil, &recent, "", 0), PruneSkip, ""},
		{"old exited with orphans is kept", mk(StateExited, &old, &old, "", 2), PruneKeep, "orphans"},
		{"old pinned is kept", mk(StateExited, &old, &old, "on-reboot", 0), PruneKeep, "pinned"},
		{"orphan scan unavailable is kept", func() *View { v := mk(StateExited, &old, &old, "", 0); v.OrphanErr = "x"; return v }(), PruneKeep, "unknown"},
		{"zero ended_at on a fresh session is not old", func() *View { z := time.Time{}; return mk(StateExited, &z, &recent, "", 0) }(), PruneSkip, ""},
		{"no timestamps at all is never pruned", mk(StateExited, nil, nil, "", 0), PruneSkip, ""},
		{"later of ended_at and created_at wins", mk(StateExited, &old, &recent, "", 0), PruneSkip, ""},
		{"recent pinned is silent", mk(StateExited, &recent, &recent, "on-reboot", 0), PruneSkip, ""},
	}
	for _, c := range cases {
		got, why := c.v.PruneDecision(now, ret)
		if got != c.want || (c.why != "" && why != c.why) {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, got, why, c.want, c.why)
		}
	}
}

func setupSession(t *testing.T, name string, ended time.Time) {
	t.Helper()
	d, err := Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(d.SessionPath(), Session{SchemaVersion: SchemaVersion, Name: name, CreatedAt: ended, Restart: "no"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(d.StatusPath(), Status{SchemaVersion: SchemaVersion, State: StateExited, EndedAt: &ended}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRemovesOnlyOldAndDryRunKeeps(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	setupSession(t, "old", time.Now().Add(-48*time.Hour))
	setupSession(t, "new", time.Now())
	r := Prune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second, DryRun: true})
	if len(r.Removed) != 1 || r.Removed[0] != "old" {
		t.Fatalf("dry-run result %+v", r)
	}
	if _, err := os.Stat(string(DirFor("old"))); err != nil {
		t.Fatal("dry-run removed the session")
	}
	r = Prune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second})
	if len(r.Removed) != 1 || len(r.Failed) != 0 {
		t.Fatalf("result %+v", r)
	}
	if _, err := os.Stat(string(DirFor("old"))); err == nil {
		t.Fatal("old session still present")
	}
	if _, err := os.Stat(string(DirFor("new"))); err != nil {
		t.Fatal("new session removed")
	}
}

func TestAutoPruneThrottle(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FASTENER_STATE_DIR", root)
	setupSession(t, "old", time.Now().Add(-48*time.Hour))
	stamp := filepath.Join(root, ".last-prune")
	opts := PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second}

	if err := os.WriteFile(stamp, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	AutoPrune(opts)
	if _, err := os.Stat(string(DirFor("old"))); err != nil {
		t.Fatal("pruned despite a fresh stamp")
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(stamp, past, past)
	AutoPrune(opts)
	if _, err := os.Stat(string(DirFor("old"))); err == nil {
		t.Fatal("not pruned with a stale stamp")
	}
	if fi, err := os.Stat(stamp); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("stamp not refreshed: %v %v", fi, err)
	}
}

func TestAutoPruneNoSessionsDirLeavesNoStamp(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FASTENER_STATE_DIR", root)
	AutoPrune(PruneOptions{Retention: time.Hour, Handshake: time.Second})
	if _, err := os.Stat(filepath.Join(root, ".last-prune")); err == nil {
		t.Fatal("stamp written with no sessions dir")
	}
}

func TestAutoPruneFutureStampIsStale(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FASTENER_STATE_DIR", root)
	setupSession(t, "old", time.Now().Add(-48*time.Hour))
	stamp := filepath.Join(root, ".last-prune")
	os.WriteFile(stamp, []byte("x\n"), 0o600)
	future := time.Now().Add(3 * time.Hour)
	os.Chtimes(stamp, future, future)
	AutoPrune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second})
	if _, err := os.Stat(string(DirFor("old"))); err == nil {
		t.Fatal("a stamp from the future suppressed pruning")
	}
}

func holdLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

func TestAutoPruneSkipsWhenLockHeld(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FASTENER_STATE_DIR", root)
	setupSession(t, "old", time.Now().Add(-48*time.Hour))
	holdLock(t, filepath.Join(root, ".last-prune"))
	AutoPrune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second})
	if _, err := os.Stat(string(DirFor("old"))); err != nil {
		t.Fatal("AutoPrune pruned while another process held the lock")
	}
}

func TestPruneLockedBusyTimesOut(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FASTENER_STATE_DIR", root)
	setupSession(t, "old", time.Now().Add(-48*time.Hour))
	holdLock(t, filepath.Join(root, ".last-prune"))
	old := lockWait
	lockWait = 150 * time.Millisecond
	defer func() { lockWait = old }()
	_, err := PruneLocked(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second})
	if !errors.Is(err, ErrPruneBusy) {
		t.Fatalf("err = %v, want ErrPruneBusy", err)
	}
	if _, err := os.Stat(string(DirFor("old"))); err != nil {
		t.Fatal("pruned without the lock")
	}
}

// The session is replaced under the same name after it was judged prunable
// and before it is deleted. The replacement must survive, untouched.
func TestPruneDoesNotDeleteReplacedSession(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	setupSession(t, "foo", time.Now().Add(-48*time.Hour))
	beforeDelete = func(name string) {
		if err := os.RemoveAll(string(DirFor(name))); err != nil {
			t.Fatal(err)
		}
		d, err := Create(name)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		WriteJSON(d.SessionPath(), Session{SchemaVersion: SchemaVersion, Name: name, CreatedAt: now, Restart: "no"})
		WriteJSON(d.StatusPath(), Status{SchemaVersion: SchemaVersion, State: StateStarting})
	}
	defer func() { beforeDelete = nil }()
	var warnings []string
	r := Prune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Hour, Warn: func(s string) { warnings = append(warnings, s) }})
	if len(r.Removed) != 0 || len(r.Failed) != 0 {
		t.Fatalf("result %+v", r)
	}
	v, err := Load("foo", time.Hour)
	if err != nil || v.State != StateStarting {
		t.Fatalf("replacement gone or changed: %v %+v", err, v)
	}
	ents, _ := os.ReadDir(SessionsDir())
	if len(ents) != 1 {
		t.Fatalf("tombstone left behind: %v (warnings %v)", ents, warnings)
	}
}

// If the name is taken again while the old copy is tombstoned and the check
// fails, the tombstone stays and a warning names it.
func TestReapLeavesTombstoneWhenNameRetaken(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	setupSession(t, "foo", time.Now().Add(-48*time.Hour))
	v, err := Load("foo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var warned string
	removed, err := Reap(v, time.Second, func(*View) bool {
		d, cerr := Create("foo") // someone recreates the name mid-check
		if cerr != nil {
			t.Fatal(cerr)
		}
		_ = d
		return false
	}, func(s string) { warned = s })
	if removed || err != nil || warned == "" {
		t.Fatalf("removed=%v err=%v warned=%q", removed, err, warned)
	}
	if _, err := os.Stat(string(DirFor("foo"))); err != nil {
		t.Fatal("new session disturbed")
	}
}

func tombstone(t *testing.T, name string, ended time.Time, age time.Duration) string {
	t.Helper()
	setupSession(t, name, ended)
	tomb := filepath.Join(SessionsDir(), ".prune-"+name+"-abcdefgh")
	if err := os.Rename(string(DirFor(name)), tomb); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-age)
	os.Chtimes(tomb, past, past)
	return tomb
}

func TestPruneSweepsLeftoverTombstones(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	tombstone(t, "stale-old", time.Now().Add(-48*time.Hour), time.Hour)
	tombstone(t, "stale-new", time.Now(), time.Hour)
	tombstone(t, "in-flight", time.Now().Add(-48*time.Hour), time.Second) // too fresh to be abandoned
	r := Prune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second})
	if len(r.Removed) != 1 || r.Removed[0] != "stale-old" {
		t.Fatalf("result %+v", r)
	}
	if _, err := os.Stat(string(DirFor("stale-new"))); err != nil {
		t.Fatal("recent tombstone not restored")
	}
	if _, err := os.Stat(string(DirFor("in-flight"))); err == nil {
		t.Fatal("tombstone in use was swept")
	}
}

func TestSweepLeavesTombstoneWhenNameTaken(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	tomb := tombstone(t, "foo", time.Now().Add(-48*time.Hour), time.Hour)
	setupSession(t, "foo", time.Now())
	var warned []string
	r := Prune(PruneOptions{Retention: 24 * time.Hour, Handshake: time.Second, Warn: func(s string) { warned = append(warned, s) }})
	if len(r.Removed) != 0 || len(warned) != 1 {
		t.Fatalf("result %+v warnings %v", r, warned)
	}
	if _, err := os.Stat(tomb); err != nil {
		t.Fatal("tombstone was removed or replaced")
	}
}

func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.Mkdir(a, 0o700)
	os.Mkdir(b, 0o700) // empty target: a plain rename would replace it
	if err := renameNoReplace(a, b); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want exists", err)
	}
	os.Remove(b)
	if err := renameNoReplace(a, b); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(a, 0o700)
	if err := renameCheckFirst(a, b); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("fallback err = %v", err)
	}
}
