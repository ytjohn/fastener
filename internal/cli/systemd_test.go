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

	"github.com/ytjohn/fastener/internal/systemd"
)

// These tests run the systemd launcher against the real user manager. They
// skip when no user manager is usable (systemd.Available: `systemctl --user
// is-system-running` is not running or degraded; GitHub's runners have no user
// systemd session), so CI never exercises them. They run in parallel with
// each other.

// userSystemd reports whether a user manager is usable here.
func userSystemd() bool {
	ok, _ := systemd.Available()
	return ok
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
	if lingers, err := systemd.Linger(); !lingers {
		t.Skipf("auto picks systemd only with linger on (linger: %v, %v)", lingers, err)
	}
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

// A SIGKILLed monitor is the unit's main process: systemd stops the unit and,
// with KillMode=mixed, SIGKILLs what is left of its cgroup at once, so even a
// grandchild that ignores SIGTERM dies promptly and no orphans remain.
func TestSystemdMonitorKilledLeavesNoOrphans(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile}, "--grace", "1s")
	v := h.waitState("s1", "running", 5*time.Second)
	gc := waitPid(t, pidfile)
	unit := h.launcherRecord("s1").Launcher.Unit
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	h.waitState("s1", "lost", 5*time.Second)
	waitPidGone(t, gc, 5*time.Second) // TimeoutStopSec is 31s: this must not wait it out
	waitUnitGone(t, unit, 5*time.Second)
	if v = h.inspect("s1"); v.State != "lost" || v.Orphans != 0 {
		t.Fatalf("after the cgroup kill: state %s, orphans %d", v.State, v.Orphans)
	}
	h.ok("rm", "s1") // nothing left to refuse over
}

// An agent that exits normally but leaves a grandchild: in systemd mode the
// grandchild (which ignores SIGTERM) is SIGKILLed with the cgroup as soon as
// the monitor exits. (setsid keeps it running and tracked as an orphan.)
func TestSystemdNormalExitKillsLeftovers(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	gc := waitPid(t, pidfile)
	h.ok("wait", "s1", "--timeout", "10s")
	waitPidGone(t, gc, 5*time.Second)
	if v := h.inspect("s1"); v.State != "exited" || v.Status.EndedBy != "exit" || v.Orphans != 0 {
		t.Fatalf("got state %s ended_by %s orphans %d", v.State, v.Status.EndedBy, v.Orphans)
	}
	h.ok("rm", "s1")
}

// stop --grace longer than the session's grace is clamped in systemd mode when
// the session has a max runtime (#5): only RuntimeMaxSec can cut a stop short.
func TestSystemdStopGraceIsClamped(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_IGNORE_TERM=1"}, "--grace", "1s", "--max-runtime", "10m")
	h.waitState("s1", "running", 5*time.Second)
	start := time.Now()
	_, errOut, code := h.cmd("stop", "s1", "--grace", "30s")
	took := time.Since(start)
	if code != 0 {
		t.Fatalf("stop exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "clamped to 1s") {
		t.Fatalf("no clamp warning: %q", errOut)
	}
	if took > 10*time.Second {
		t.Fatalf("stop took %s: --grace was not clamped", took)
	}
	if v := h.inspect("s1"); v.Status.EndedBy != "stop" || v.Status.Signal != "SIGKILL" {
		t.Fatalf("got %+v", v.Status)
	}
	// A shorter --grace is not touched.
	h.start("s2", []string{"FAKE_SLEEP=60s"}, "--grace", "5s", "--max-runtime", "10m")
	h.waitState("s2", "running", 5*time.Second)
	if _, errOut, code := h.cmd("stop", "s2", "--grace", "1s"); code != 0 || strings.Contains(errOut, "clamped") {
		t.Fatalf("shorter grace: exit %d: %q", code, errOut)
	}
	// Without a max runtime there is no RuntimeMaxSec, so no clamp: the
	// longer grace is honoured (the agent ignores TERM, so stop waits it out).
	h.start("s3", []string{"FAKE_SLEEP=60s", "FAKE_IGNORE_TERM=1"}, "--grace", "1s")
	h.waitState("s3", "running", 5*time.Second)
	start = time.Now()
	_, errOut, code = h.cmd("stop", "s3", "--grace", "3s")
	if took := time.Since(start); code != 0 || strings.Contains(errOut, "clamped") || took < 3*time.Second {
		t.Fatalf("no max runtime: exit %d after %s: %q", code, took, errOut)
	}
}

// The agent gets the caller's open-file limit in both modes, not the user
// manager's default soft limit of 1024.
func TestSystemdAgentNofileMatchesSetsid(t *testing.T) {
	sd := newSystemdHarness(t, "systemd")
	ss := newHarness(t)
	reply := func(h *harness, name string) string {
		h.start(name, []string{"FAKE_REPLY_RLIMIT=1"})
		h.ok("wait", name, "--timeout", "10s")
		return h.inspect(name).Status.FinalMessage
	}
	for _, soft := range []string{"", "4096"} {
		sd.softNofile, ss.softNofile = soft, soft
		name := "n" + soft
		got, want := reply(sd, name), reply(ss, name)
		t.Logf("caller soft NOFILE %q: agent sees %s under systemd, %s under setsid", soft, got, want)
		if got != want || want == "" || strings.HasPrefix(want, "error") {
			t.Fatalf("caller soft %q: systemd agent NOFILE %q, setsid %q", soft, got, want)
		}
		if soft != "" && got != soft {
			t.Fatalf("agent NOFILE %q, want the caller's %s", got, soft)
		}
	}
}

// The setsid launcher never clamps.
func TestSetsidStopGraceNotClamped(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=60s"}, "--grace", "1s")
	h.waitState("s1", "running", 5*time.Second)
	if _, errOut, code := h.cmd("stop", "s1", "--grace", "3s"); code != 0 || strings.Contains(errOut, "clamped") {
		t.Fatalf("exit %d: %q", code, errOut)
	}
}
