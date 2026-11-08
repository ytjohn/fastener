package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run the systemd launcher against the real user manager. They
// skip when `systemctl --user is-system-running` fails (GitHub's runners have
// no user systemd session), so CI never exercises them. They run in parallel
// with each other because two of them wait out systemd's stop timeout.

// userSystemd reports whether a user manager is usable here.
func userSystemd() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "is-system-running").Run() == nil
}

// newSystemdHarness is a harness whose sessions use launcher (systemd or auto),
// with the bus variables systemctl needs passed through. It skips without a
// user manager, and on cleanup checks that every unit it started is gone.
func newSystemdHarness(t *testing.T, launcher string) *harness {
	t.Helper()
	if !userSystemd() {
		t.Skip("no user systemd manager (systemctl --user is-system-running failed)")
	}
	t.Parallel()
	h := newHarness(t)
	h.launcher = launcher
	h.writeConfig("")
	for _, k := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(k); ok {
			h.extraEnv = append(h.extraEnv, k+"="+v)
		}
	}
	// Registered after newHarness's cleanup, so it runs first: collect the
	// units before rm --force deletes the records, then check them afterwards.
	t.Cleanup(func() {
		units := h.units()
		h.cleanup()
		for _, u := range units {
			waitUnitGone(t, u, 45*time.Second)
		}
	})
	return h
}

type launcherJSON struct {
	Launcher struct {
		Launcher string `json:"launcher"`
		Unit     string `json:"unit"`
		Monitor  *struct {
			PID int `json:"pid"`
		} `json:"monitor"`
	} `json:"launcher"`
}

func (h *harness) launcherRecord(name string) launcherJSON {
	h.t.Helper()
	var v launcherJSON
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

// units lists the units recorded by every session in the harness's state dir.
func (h *harness) units() []string {
	paths, _ := filepath.Glob(filepath.Join(h.state, "sessions", "*", "generations", "*.launcher.json"))
	var out []string
	for _, p := range paths {
		var l struct {
			Unit string `json:"unit"`
		}
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &l) == nil && l.Unit != "" {
			out = append(out, l.Unit)
		}
	}
	return out
}

// unitGone reports whether unit is inactive or collected.
func unitGone(t *testing.T, unit string) bool {
	t.Helper()
	out, err := exec.Command("systemctl", "--user", "show", unit, "-p", "ActiveState", "-p", "LoadState").Output()
	if err != nil {
		t.Fatalf("systemctl show %s: %v", unit, err)
	}
	s := string(out)
	return strings.Contains(s, "LoadState=not-found") || strings.Contains(s, "ActiveState=inactive")
}

func waitUnitGone(t *testing.T, unit string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !unitGone(t, unit) {
		if time.Now().After(deadline) {
			t.Errorf("unit %s still loaded after %s", unit, timeout)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitPidGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after %s", pid, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSystemdRunToExit(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	h.start("s1", []string{"FAKE_SLEEP=300ms"})
	rec := h.launcherRecord("s1")
	if rec.Launcher.Launcher != "systemd" || !strings.HasPrefix(rec.Launcher.Unit, "fastener-s1-g1-") ||
		!strings.HasSuffix(rec.Launcher.Unit, ".service") || rec.Launcher.Monitor == nil {
		t.Fatalf("launcher record %+v", rec.Launcher)
	}
	if _, _, code := h.cmd("wait", "s1", "--timeout", "10s"); code != 0 {
		t.Fatalf("wait exit %d", code)
	}
	v := h.inspect("s1")
	if v.State != "exited" || v.Status.EndedBy != "exit" || v.Status.ExitCode == nil || *v.Status.ExitCode != 0 ||
		v.Status.FinalMessage != "pong" {
		t.Fatalf("got %+v", v)
	}
	if v.Status.Monitor == nil || v.Status.Monitor.PID != rec.Launcher.Monitor.PID {
		t.Fatalf("status monitor %+v, launcher monitor %+v", v.Status.Monitor, rec.Launcher.Monitor)
	}
	// --collect: the unit goes away once the monitor exits.
	waitUnitGone(t, rec.Launcher.Unit, 10*time.Second)
}

func TestSystemdAutoPicksSystemd(t *testing.T) {
	h := newSystemdHarness(t, "auto")
	h.start("s1", nil)
	if rec := h.launcherRecord("s1"); rec.Launcher.Launcher != "systemd" || rec.Launcher.Unit == "" {
		t.Fatalf("auto chose %+v, want systemd", rec.Launcher)
	}
	h.ok("wait", "s1", "--timeout", "10s")
}

func TestSystemdStopIgnoringAgentAndGrandchild(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_IGNORE_TERM=1", "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	h.waitState("s1", "running", 5*time.Second)
	gc := waitPid(t, pidfile)
	start := time.Now()
	h.ok("stop", "s1", "--grace", "1s")
	took := time.Since(start)
	v := h.inspect("s1")
	if v.Status.EndedBy != "stop" || v.Status.Signal != "SIGKILL" {
		t.Fatalf("got %+v", v.Status)
	}
	if took < time.Second || took > 8*time.Second {
		t.Fatalf("stop took %s with --grace 1s", took)
	}
	waitPidGone(t, gc, 5*time.Second)
	waitUnitGone(t, h.launcherRecord("s1").Launcher.Unit, 10*time.Second)
}

func TestSystemdMaxRuntime(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	h.start("s1", []string{"FAKE_SLEEP=60s"}, "--max-runtime", "1s", "--grace", "1s")
	_, _, code := h.cmd("wait", "s1", "--timeout", "15s")
	v := h.inspect("s1")
	if v.Status.EndedBy != "limit" || v.Status.Signal != "SIGTERM" || code != 128+15 {
		t.Fatalf("code %d, %+v", code, v.Status)
	}
}

func TestSystemdExternalSigtermIsSignal(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	h.start("s1", []string{"FAKE_SLEEP=60s"})
	v := h.waitState("s1", "running", 5*time.Second)
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGTERM)
	if v = h.waitState("s1", "exited", 15*time.Second); v.Status.EndedBy != "signal" {
		t.Fatalf("ended_by %q, want signal", v.Status.EndedBy)
	}
}

// A SIGKILLed monitor is the unit's main process: systemd stops the unit and
// kills its cgroup, so even a grandchild that ignores SIGTERM dies (after
// TimeoutStopSec = grace + 30s), with no orphans left for stop or rm.
func TestSystemdMonitorKilledLeavesNoOrphans(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile}, "--grace", "1s")
	v := h.waitState("s1", "running", 5*time.Second)
	gc := waitPid(t, pidfile)
	unit := h.launcherRecord("s1").Launcher.Unit
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	h.waitState("s1", "lost", 5*time.Second)
	waitPidGone(t, gc, 45*time.Second)
	waitUnitGone(t, unit, 10*time.Second)
	if v = h.inspect("s1"); v.State != "lost" || v.Orphans != 0 {
		t.Fatalf("after the cgroup kill: state %s, orphans %d", v.State, v.Orphans)
	}
	h.ok("rm", "s1") // nothing left to refuse over
}
