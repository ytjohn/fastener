package session

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
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
	stale := Status{State: StateRunning, Generation: 1, Monitor: &dead, BootID: boot}
	stale.RemoteURL, stale.RemoteSessions = "https://claude.ai/code?environment=env_x", []string{"https://claude.ai/code/session_x"}
	WriteJSON(lost.StatusPath(), stale)
	for _, d := range []Dir{lost, exited} {
		if err := MarkRestoreFailed(d, "no_remote", "backend has no [remote]"); err != nil {
			t.Fatal(err)
		}
		v, err := LoadAt(d, "x", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != StateRestoreFailed || v.Status.RestoreReason != "no_remote" || v.Status.Error != "backend has no [remote]" || v.Status.EndedAt == nil ||
			v.Status.RemoteURL != "" || v.Status.RemoteSessions != nil {
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

// TestRestoredStartingGraceExpires: a restored generation planned long ago
// with no monitor record is lost.
func TestRestoredStartingGraceExpires(t *testing.T) {
	setup(t)
	d := writeSession(t, "rs-old", time.Now().Add(-48*time.Hour), Status{State: StateStarting, Generation: 2}, nil)
	if err := WriteJSON(d.PlanPath(2), Plan{SchemaVersion: 1, Generation: 2, Marker: "rs-old:2:x", PlannedAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	v, err := Load("rs-old", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != StateLost {
		t.Errorf("state = %s, want lost", v.State)
	}
}

// markedSleeper starts a process carrying marker in a process group of its
// own, so only an any-group scan finds it.
func markedSleeper(t *testing.T, marker string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "30")
	c.Env = append(os.Environ(), proc.MarkerEnv+"="+marker)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c
}

func TestMarkedAnyGroupAndBoot(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	markedSleeper(t, "anyg:1:x")
	// Lost, recording a process group the sleeper is not in.
	d := writeSession(t, "anyg", time.Now(), Status{State: StateRunning, Generation: 1, Monitor: &dead, BootID: boot, PGID: 999999}, nil)
	v, err := LoadFull("anyg", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != StateLost || len(v.Orphans) != 1 || v.OrphanErr != "" {
		t.Fatalf("lost: state %s, orphans %v, err %q", v.State, v.Orphans, v.OrphanErr)
	}
	// A previous boot: nothing is scanned, so the (current-boot) process is not counted.
	old := currentBoot
	t.Cleanup(func() { currentBoot = old })
	currentBoot = func() (string, error) { return "a-later-boot", nil }
	if ids, err := v.MarkedAnyGroup(); err != nil || len(ids) != 0 {
		t.Errorf("previous boot: %v, %v", ids, err)
	}
	currentBoot = old

	// restore-failed with no process group recorded: the any-group scan.
	WriteJSON(d.StatusPath(), Status{State: StateRestoreFailed, Generation: 1, BootID: boot})
	if v, _ := LoadFull("anyg", time.Second); len(v.Orphans) != 1 {
		t.Errorf("restore-failed, no pgid: orphans %v (%s)", v.Orphans, v.OrphanErr)
	}
	// With a process group recorded: only that group is scanned.
	WriteJSON(d.StatusPath(), Status{State: StateRestoreFailed, Generation: 1, BootID: boot, PGID: 999999})
	if v, _ := LoadFull("anyg", time.Second); len(v.Orphans) != 0 || v.OrphanErr != "" {
		t.Errorf("restore-failed, other pgid: orphans %v (%s)", v.Orphans, v.OrphanErr)
	}
	// No plan for a generation that spawned something: unknown.
	os.Remove(d.PlanPath(1))
	WriteJSON(d.StatusPath(), Status{State: StateRunning, Generation: 1, Monitor: &dead, BootID: boot, PGID: 999999})
	if v, _ := LoadFull("anyg", time.Second); v.OrphanErr == "" {
		t.Error("lost without a plan: no orphan error")
	}
}
