package session

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

func TestLockRestoreExclusive(t *testing.T) {
	setup(t)
	d := writeSession(t, "rl", time.Now(), Status{State: StateRunning}, nil)
	unlock, err := LockRestore(d, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := LockRestore(d, 150*time.Millisecond); !errors.Is(err, ErrRestoreBusy) {
		t.Fatalf("second lock: %v, want ErrRestoreBusy", err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Error("second lock gave up before its wait")
	}
	// The restore lock is not the finalize lock: Finalize still works.
	if _, err := Finalize(d, func(s *Status) { s.State = StateExited }); err != nil {
		t.Fatalf("Finalize under the restore lock: %v", err)
	}
	unlock()
	unlock2, err := LockRestore(d, time.Second)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	unlock2()
	if _, err := LockRestore(Dir(string(d)+"-gone"), time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestRotateLogs(t *testing.T) {
	setup(t)
	d := writeSession(t, "rot", time.Now(), Status{State: StateRunning}, nil)
	os.WriteFile(d.OutputLog(), []byte("gen1 out\n"), 0o600)
	os.WriteFile(d.StderrLog(), []byte("gen1 err\n"), 0o600)
	if err := RotateLogs(d, 1); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{d.OutputLogGen(1): "gen1 out\n", d.StderrLogGen(1): "gen1 err\n"} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", path, b, err, want)
		}
	}
	for _, p := range []string{d.OutputLog(), d.StderrLog()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	// Nothing to rotate is fine; an existing rotated log is appended to.
	if err := RotateLogs(d, 2); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(d.OutputLog(), []byte("more\n"), 0o600)
	if err := RotateLogs(d, 1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(d.OutputLogGen(1)); string(b) != "gen1 out\nmore\n" {
		t.Errorf("appended rotation = %q", b)
	}
}

func TestMarkRestoreFailed(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	code := 1
	// A lost record and an exited one (a relaunch that ended at once) both
	// become restore-failed; the exit facts are kept.
	lost := writeSession(t, "rf-lost", time.Now(), Status{State: StateRunning, Generation: 1, Monitor: &dead, BootID: boot}, nil)
	ended := time.Now().UTC().Add(-time.Minute)
	exited := writeSession(t, "rf-exited", time.Now(), Status{State: StateExited, Generation: 2, EndedBy: EndedByExit, ExitCode: &code, EndedAt: &ended}, nil)
	for _, d := range []Dir{lost, exited} {
		if err := MarkRestoreFailed(d, "no_remote", "backend has no [remote]"); err != nil {
			t.Fatal(err)
		}
		v, err := LoadAt(d, "x", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != StateRestoreFailed || v.Status.RestoreReason != "no_remote" || v.Status.Error != "backend has no [remote]" || v.Status.EndedAt == nil {
			t.Errorf("%s: %+v", d, v.Status)
		}
	}
	var st Status
	ReadJSON(exited.StatusPath(), &st)
	if st.Generation != 2 || st.EndedBy != EndedByExit || st.ExitCode == nil || *st.ExitCode != 1 || !st.EndedAt.Equal(ended) {
		t.Errorf("exit facts lost: %+v", st)
	}
	// A live monitor is never overwritten.
	self, _ := proc.Self()
	live := writeSession(t, "rf-live", time.Now(), Status{State: StateRunning, Monitor: &self, BootID: boot}, nil)
	if err := MarkRestoreFailed(live, "x", "y"); !errors.Is(err, ErrMonitorAlive) {
		t.Fatalf("live monitor: %v", err)
	}
}

// TestRestoredStartingGrace: a restored generation that is starting with no
// monitor record yet gets 2×handshake from its plan, not from the session's
// (long past) creation.
func TestRestoredStartingGrace(t *testing.T) {
	setup(t)
	d := writeSession(t, "rs", time.Now().Add(-48*time.Hour), Status{State: StateStarting, Generation: 2}, nil)
	if err := WriteJSON(d.PlanPath(2), Plan{SchemaVersion: 1, Generation: 2, Marker: "rs:2:x", PlannedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	v, err := Load("rs", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != StateStarting {
		t.Errorf("state = %s, want starting", v.State)
	}
}
