package session

import (
	"os"
	"path/filepath"
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
		{"old exited", mk(StateExited, &old, &recent, "no", 0), PruneRemove, ""},
		{"old restore-failed", mk(StateRestoreFailed, &old, &recent, "", 0), PruneRemove, ""},
		{"recent exited", mk(StateExited, &recent, &old, "", 0), PruneSkip, ""},
		{"exited without ended_at uses created_at", mk(StateExited, nil, &old, "", 0), PruneRemove, ""},
		{"running is never touched", mk(StateRunning, nil, &old, "", 0), PruneSkip, ""},
		{"starting is never touched", mk(StateStarting, nil, &old, "", 0), PruneSkip, ""},
		{"old lost is kept", mk(StateLost, nil, &old, "", 0), PruneKeep, "lost"},
		{"recent lost is silent", mk(StateLost, nil, &recent, "", 0), PruneSkip, ""},
		{"old exited with orphans is kept", mk(StateExited, &old, &old, "", 2), PruneKeep, "orphans"},
		{"old pinned is kept", mk(StateExited, &old, &old, "on-reboot", 0), PruneKeep, "pinned"},
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
