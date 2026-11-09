package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const fakeBridgeURL = "https://claude.ai/code?environment=env_01FAKEBRIDGEFAKEBRIDGE01"

type restoreJSON struct {
	Name       string `json:"name"`
	Result     string `json:"result"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	Generation int    `json:"generation"`
	RemoteURL  string `json:"remote_url"`
}

// restore runs `restore --json args...` and decodes its output.
func (h *harness) restore(wantCode int, args ...string) []restoreJSON {
	h.t.Helper()
	out, errOut, code := h.cmd(append([]string{"restore", "--json"}, args...)...)
	if code != wantCode {
		h.t.Fatalf("restore %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, wantCode, out, errOut)
	}
	var rs []restoreJSON
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		h.t.Fatalf("restore %v: %v: %q", args, err, out)
	}
	return rs
}

func (h *harness) sessDir(name string) string { return filepath.Join(h.state, "sessions", name) }

func (h *harness) readFile(name, rel string) []byte {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.sessDir(name), rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

type planJSON struct {
	Generation int      `json:"generation"`
	Marker     string   `json:"marker"`
	Argv       []string `json:"argv"`
}

func (h *harness) plan(name string, gen int) planJSON {
	h.t.Helper()
	var p planJSON
	if err := json.Unmarshal(h.readFile(name, fmt.Sprintf("generations/%d.json", gen)), &p); err != nil {
		h.t.Fatal(err)
	}
	return p
}

type restoreInspect struct {
	State  string `json:"state"`
	Status struct {
		Generation    int    `json:"generation"`
		RemoteURL     string `json:"remote_url"`
		RestoreReason string `json:"restore_reason"`
		Error         string `json:"error"`
	} `json:"status"`
}

func (h *harness) restoreInspect(name string) restoreInspect {
	h.t.Helper()
	var v restoreInspect
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

func TestRestoreRelaunchesBridge(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-r")
	sessBefore := h.readFile("br-r", "session.json")
	p1 := h.plan("br-r", 1)
	h.simulateReboot("br-r")
	gen1Out := h.readFile("br-r", "output.log")
	if !bytes.Contains(gen1Out, []byte("env_01FAKEBRIDGEFAKEBRIDGE01")) {
		t.Fatalf("generation 1 output.log lacks the URL: %q", gen1Out)
	}

	rs := h.restore(0)
	if len(rs) != 1 || rs[0] != (restoreJSON{Name: "br-r", Result: "restored", Generation: 2, RemoteURL: fakeBridgeURL}) {
		t.Fatalf("restore = %+v", rs)
	}
	// The --json shape: exactly these keys.
	out, _, _ := h.cmd("restore", "--json", "--dry-run", "br-r")
	var raw []map[string]any
	json.Unmarshal([]byte(out), &raw)
	if len(raw) != 1 || raw[0]["name"] != "br-r" || raw[0]["result"] != "skipped" || raw[0]["reason"] != "running" {
		t.Errorf("dry-run of a running session = %s", out)
	}

	p2 := h.plan("br-r", 2)
	if p2.Generation != 2 || p2.Marker == p1.Marker || !strings.HasPrefix(p2.Marker, "br-r:2:") {
		t.Errorf("plan 2 = %+v (plan 1 marker %s)", p2, p1.Marker)
	}
	if strings.Join(p2.Argv, " ") != strings.Join(p1.Argv, " ") {
		t.Errorf("argv changed: %v -> %v", p1.Argv, p2.Argv)
	}
	if !exists(filepath.Join(h.sessDir("br-r"), "generations", "2.launcher.json")) {
		t.Error("no launcher record for generation 2")
	}
	if got := h.readFile("br-r", "output.1.log"); !bytes.Equal(got, gen1Out) {
		t.Errorf("output.1.log = %q, want generation 1's output", got)
	}
	if !exists(filepath.Join(h.sessDir("br-r"), "stderr.1.log")) {
		t.Error("stderr.log not rotated")
	}
	v := h.restoreInspect("br-r")
	if v.State != "running" || v.Status.Generation != 2 || v.Status.RemoteURL != fakeBridgeURL {
		t.Errorf("inspect = %+v", v)
	}
	if !bytes.Equal(h.readFile("br-r", "session.json"), sessBefore) {
		t.Error("session.json changed")
	}

	// logs --generation.
	if got := h.ok("logs", "br-r", "--generation", "1"); got != strings.TrimSpace(string(gen1Out)) {
		t.Errorf("logs --generation 1 = %q", got)
	}
	cur := h.ok("logs", "br-r")
	if g2 := h.ok("logs", "br-r", "--generation", "2"); g2 != cur || !strings.Contains(cur, "env_01FAKEBRIDGEFAKEBRIDGE01") {
		t.Errorf("logs --generation 2 = %q, logs = %q", g2, cur)
	}
	if _, errOut, code := h.cmd("logs", "br-r", "--generation", "3"); code != 1 || !strings.Contains(errOut, "no generation 3") {
		t.Errorf("logs --generation 3: exit %d, %q", code, errOut)
	}

	// Once stopped it is not lost: restore leaves it, and says why if named.
	h.ok("stop", "br-r")
	h.waitState("br-r", "exited", 10*time.Second)
	if rs := h.restore(0); len(rs) != 0 {
		t.Errorf("restore of nothing = %+v", rs)
	}
	if rs := h.restore(0, "br-r"); len(rs) != 1 || rs[0].Result != "skipped" || rs[0].Reason != "not_lost" {
		t.Errorf("restore of a stopped session = %+v", rs)
	}
	h.ok("rm", "br-r")
}

func TestRestoreSecondGeneration(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-g")
	h.simulateReboot("br-g")
	h.restore(0)
	h.simulateReboot("br-g")
	rs := h.restore(0)
	if len(rs) != 1 || rs[0].Result != "restored" || rs[0].Generation != 3 {
		t.Fatalf("restore = %+v", rs)
	}
	for _, f := range []string{"output.1.log", "output.2.log", "generations/3.json"} {
		if !exists(filepath.Join(h.sessDir("br-g"), f)) {
			t.Errorf("%s missing", f)
		}
	}
	// A stop for generation 3 still works (the old request files are gone).
	h.ok("stop", "br-g")
	h.waitState("br-g", "exited", 10*time.Second)
}

func TestRestoreSkips(t *testing.T) {
	h := newBridgeHarness(t)
	// Still running: named, it is skipped; unnamed, it is not a candidate.
	h.startOnReboot("br-live")
	if rs := h.restore(0); len(rs) != 0 {
		t.Errorf("restore with only a running session = %+v", rs)
	}
	if rs := h.restore(0, "br-live"); len(rs) != 1 || rs[0].Result != "skipped" || rs[0].Reason != "running" {
		t.Errorf("restore br-live = %+v", rs)
	}
	h.ok("stop", "br-live")

	// Lost with orphans alive: the monitor dies (taking the agent with it on
	// Linux), but a grandchild that ignores SIGTERM survives.
	h.startOnReboot("br-orphan", "FAKE_GRANDCHILD=1", "--grace=300ms")
	mon := h.inspect("br-orphan").Status.Monitor.PID
	syscall.Kill(mon, syscall.SIGKILL)
	h.waitState("br-orphan", "lost", 10*time.Second)
	rs := h.restore(0)
	if len(rs) != 1 || rs[0].Name != "br-orphan" || rs[0].Result != "skipped" || rs[0].Reason != "orphans" ||
		!strings.Contains(rs[0].Message, "fastener stop br-orphan") {
		t.Errorf("restore with orphans = %+v", rs)
	}
	if exists(filepath.Join(h.sessDir("br-orphan"), "generations", "2.json")) {
		t.Error("a generation 2 was planned beside live orphans")
	}
	if !strings.Contains(rs[0].Message, "then fastener restore br-orphan") {
		t.Errorf("orphans message = %q", rs[0].Message)
	}
	// The way out the message names: stop cleans them up and finalizes, after
	// which an unnamed restore leaves the session alone, and a named one
	// relaunches it.
	h.ok("stop", "br-orphan")
	if v := h.inspect("br-orphan"); v.State != "exited" || v.Orphans != 0 || v.Status.EndedBy != "stop" {
		t.Fatalf("after stop: %+v", v)
	}
	if rs := h.restore(0); len(rs) != 0 {
		t.Errorf("unnamed restore took a stopped session: %+v", rs)
	}
	if rs := h.restore(0, "br-orphan"); len(rs) != 1 || rs[0].Result != "restored" || rs[0].Generation != 2 || rs[0].RemoteURL != fakeBridgeURL {
		t.Errorf("named restore after stop = %+v", rs)
	}
	h.ok("stop", "br-orphan")
	// A session stopped normally (not finalized from lost) is still refused.
	if rs := h.restore(0, "br-orphan"); len(rs) != 1 || rs[0].Reason != "not_lost" {
		t.Errorf("restore of a normally stopped session = %+v", rs)
	}

	// Headless sessions are never restored.
	h.start("hl", []string{"FAKE_SLEEP=1h"})
	h.waitState("hl", "running", 10*time.Second)
	h.simulateReboot("hl")
	if rs := h.restore(0); len(rs) != 0 {
		t.Errorf("restore picked up a headless session: %+v", rs)
	}
	if rs := h.restore(0, "hl"); len(rs) != 1 || rs[0].Result != "skipped" || rs[0].Reason != "not_on_reboot" {
		t.Errorf("restore hl = %+v", rs)
	}
	// Even one whose session.json was edited to claim on-reboot.
	sp := filepath.Join(h.sessDir("hl"), "session.json")
	b, _ := os.ReadFile(sp)
	os.WriteFile(sp, bytes.Replace(b, []byte(`"restart": "no"`), []byte(`"restart": "on-reboot"`), 1), 0o600)
	if rs := h.restore(0); len(rs) != 1 || rs[0].Result != "skipped" || rs[0].Reason != "not_remote" {
		t.Errorf("restore of an on-reboot headless session = %+v", rs)
	}
	if exists(filepath.Join(h.sessDir("hl"), "generations", "2.json")) {
		t.Error("a headless session was relaunched")
	}

	if _, errOut, code := h.cmd("restore", "Bad Name"); code != 2 || !strings.Contains(errOut, "invalid session name") {
		t.Errorf("bad name: exit %d %q", code, errOut)
	}
	if rs := h.restore(1, "nope"); len(rs) != 1 || rs[0].Result != "failed" || rs[0].Reason != "not_found" {
		t.Errorf("unknown name = %+v", rs)
	}
}

func TestRestoreFailedProfile(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-norem")
	// br-noperm uses skip, which a later profile drops.
	h.ok("run", "fakebridge", "--remote", "--restart", "on-reboot", "--cwd", h.work, "--name", "br-noperm",
		"--permissions", "skip", "--env", "FAKE_BRIDGE=1")
	h.simulateReboot("br-norem")
	h.simulateReboot("br-noperm")
	gen1 := h.readFile("br-norem", "output.log")

	full := fmt.Sprintf(fakeBridgeProfile, fakeBin)
	noRemote := full[:strings.Index(full, "[remote]")]
	h.writeProfile("fakebridge", noRemote)
	rs := h.restore(1)
	got := map[string]restoreJSON{}
	for _, r := range rs {
		got[r.Name] = r
	}
	for _, n := range []string{"br-norem", "br-noperm"} {
		if r := got[n]; r.Result != "failed" || r.Reason != "no_remote" || !strings.Contains(r.Message, "no [remote] section") {
			t.Errorf("%s: %+v", n, r)
		}
	}
	v := h.restoreInspect("br-norem")
	if v.State != "restore-failed" || v.Status.RestoreReason != "no_remote" || v.Status.Generation != 1 || v.Status.RemoteURL != "" {
		t.Errorf("inspect = %+v", v)
	}
	if exists(filepath.Join(h.sessDir("br-norem"), "generations", "2.json")) || !bytes.Equal(h.readFile("br-norem", "output.log"), gen1) {
		t.Error("a failed restore planned a generation or rotated the logs")
	}
	var rows []struct {
		Name, State string
	}
	json.Unmarshal([]byte(h.ok("ps", "--json")), &rows)
	for _, r := range rows {
		if r.State != "restore-failed" {
			t.Errorf("ps: %s is %s", r.Name, r.State)
		}
	}
	if human := h.ok("inspect", "br-norem"); !strings.Contains(human, "restore_reason  no_remote") {
		t.Errorf("inspect lacks restore_reason:\n%s", human)
	}
	// restore-failed is not lost: an unnamed restore leaves it alone.
	if rs := h.restore(0); len(rs) != 0 {
		t.Errorf("unnamed restore retried restore-failed sessions: %+v", rs)
	}

	// Without the skip level.
	noSkip := strings.Replace(full, "skip = [\"--skip\"]\n", "", 1)
	noSkip = strings.Replace(noSkip, "\n[remote.permissions]\nskip = [\"--permission-mode\", \"bypassPermissions\"]\n", "\n", 1)
	h.writeProfile("fakebridge", noSkip)
	if rs := h.restore(1, "br-noperm"); len(rs) != 1 || rs[0].Reason != "no_permission_level" {
		t.Errorf("restore br-noperm = %+v", rs)
	}
	if v := h.restoreInspect("br-noperm"); v.State != "restore-failed" || v.Status.RestoreReason != "no_permission_level" {
		t.Errorf("inspect br-noperm = %+v", v)
	}

	// Fixed profile: naming the session retries it.
	h.writeProfile("fakebridge", full)
	if rs := h.restore(0, "br-norem", "br-noperm"); len(rs) != 2 || rs[0].Result != "restored" || rs[1].Result != "restored" || rs[0].Generation != 2 {
		t.Errorf("retry = %+v", rs)
	}
	if v := h.restoreInspect("br-norem"); v.State != "running" || v.Status.RestoreReason != "" {
		t.Errorf("after retry: %+v", v)
	}

	// A binary that is gone.
	h.simulateReboot("br-norem")
	h.writeProfile("fakebridge", strings.Replace(full, fmt.Sprintf("%q", fakeBin), `"/nonexistent/fakeagent"`, 1))
	if rs := h.restore(1, "br-norem"); len(rs) != 1 || rs[0].Reason != "binary_not_found" {
		t.Errorf("missing binary = %+v", rs)
	}
}

func TestRestoreUntrustedFolder(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-trust")
	h.simulateReboot("br-trust")
	// The folder is no longer trusted: the relaunched bridge refuses it.
	wrapper := filepath.Join(t.TempDir(), "untrusted-agent")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nFAKE_UNTRUSTED=1 exec "+fakeBin+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.writeProfile("fakebridge", strings.Replace(fmt.Sprintf(fakeBridgeProfile, fakeBin), fmt.Sprintf("%q", fakeBin), fmt.Sprintf("%q", wrapper), 1))
	out, errOut, code := h.cmd("restore")
	want := "br-trust  failed  generation 2: " + h.work + " is not a trusted workspace for untrusted-agent bridge; authorize it with: trust-it " + h.work
	if code != 1 || strings.TrimSpace(out) != want {
		t.Errorf("exit %d\nstdout %q\nwant   %q\nstderr %q", code, out, want, errOut)
	}
	v := h.restoreInspect("br-trust")
	if v.State != "restore-failed" || v.Status.RestoreReason != "untrusted_workspace" || v.Status.Generation != 2 ||
		!strings.Contains(v.Status.Error, "trust-it "+h.work) {
		t.Errorf("inspect = %+v", v)
	}
	// After the folder is trusted again, naming it restores generation 3.
	h.writeProfile("fakebridge", fmt.Sprintf(fakeBridgeProfile, fakeBin))
	if rs := h.restore(0, "br-trust"); len(rs) != 1 || rs[0].Result != "restored" || rs[0].Generation != 3 || rs[0].RemoteURL != fakeBridgeURL {
		t.Errorf("retry = %+v", rs)
	}
	if !exists(filepath.Join(h.sessDir("br-trust"), "stderr.2.log")) {
		t.Error("generation 2's stderr not rotated")
	}
}

func TestRestoreConcurrent(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-c")
	h.simulateReboot("br-c")
	var wg sync.WaitGroup
	outs := make([]string, 4)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], _, _ = h.run("restore", "--json", "br-c")
		}(i)
	}
	wg.Wait()
	restored := 0
	for _, o := range outs {
		var rs []restoreJSON
		if err := json.Unmarshal([]byte(o), &rs); err != nil || len(rs) != 1 {
			t.Fatalf("output %q: %v", o, err)
		}
		switch r := rs[0]; {
		case r.Result == "restored":
			restored++
		case r.Result != "skipped" || r.Reason != "running":
			t.Errorf("concurrent restore: %+v", r)
		}
	}
	if restored != 1 {
		t.Errorf("%d restores relaunched, want exactly 1: %v", restored, outs)
	}
	plans, _ := filepath.Glob(filepath.Join(h.sessDir("br-c"), "generations", "*.json"))
	if len(plans) != 4 { // 1.json, 1.launcher.json, 2.json, 2.launcher.json
		t.Errorf("generation files: %v", plans)
	}
}

func TestRestoreDryRun(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-dry")
	h.simulateReboot("br-dry")
	before := h.readFile("br-dry", "output.log")
	rs := h.restore(0, "--dry-run")
	if len(rs) != 1 || rs[0] != (restoreJSON{Name: "br-dry", Result: "would-restore", Generation: 2}) {
		t.Errorf("dry run = %+v", rs)
	}
	if out := h.ok("restore", "--dry-run"); out != "br-dry  would-restore  generation 2" {
		t.Errorf("dry run (human) = %q", out)
	}
	full := fmt.Sprintf(fakeBridgeProfile, fakeBin)
	h.writeProfile("fakebridge", full[:strings.Index(full, "[remote]")])
	if rs := h.restore(1, "--dry-run"); len(rs) != 1 || rs[0].Result != "would-fail" || rs[0].Reason != "no_remote" {
		t.Errorf("dry run, no [remote] = %+v", rs)
	}
	if v := h.restoreInspect("br-dry"); v.State != "lost" || v.Status.Generation != 1 {
		t.Errorf("dry run changed the session: %+v", v)
	}
	if exists(filepath.Join(h.sessDir("br-dry"), "generations", "2.json")) || !bytes.Equal(h.readFile("br-dry", "output.log"), before) ||
		exists(filepath.Join(h.sessDir("br-dry"), "restore.lock")) {
		t.Error("dry run acted")
	}
}

type launcherKind struct {
	Launcher struct {
		Launcher string `json:"launcher"`
	} `json:"launcher"`
}

func (h *harness) launcherKind(name string) string {
	h.t.Helper()
	var v launcherKind
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		h.t.Fatal(err)
	}
	return v.Launcher.Launcher
}

// TestRestoreKeepsSetsidLauncher: a bridge started with setsid is relaunched
// with setsid, although the config now says systemd.
func TestRestoreKeepsSetsidLauncher(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-setsid")
	h.simulateReboot("br-setsid")
	h.launcher = "systemd"
	h.writeConfig("")
	if rs := h.restore(0); len(rs) != 1 || rs[0].Result != "restored" {
		t.Fatalf("restore = %+v", rs)
	}
	if k := h.launcherKind("br-setsid"); k != "setsid" {
		t.Errorf("generation 2 launcher = %s, want setsid", k)
	}
}

// TestRestoreKeepsSystemdLauncher: the reverse, against the real user
// manager (skipped without one).
func TestRestoreKeepsSystemdLauncher(t *testing.T) {
	h := newSystemdHarness(t, "systemd")
	h.writeProfile("fakebridge", fmt.Sprintf(fakeBridgeProfile, fakeBin))
	h.startOnReboot("br-sysd")
	if k := h.launcherKind("br-sysd"); k != "systemd" {
		t.Fatalf("generation 1 launcher = %s", k)
	}
	h.simulateReboot("br-sysd")
	h.launcher = "setsid"
	h.writeConfig("")
	if rs := h.restore(0); len(rs) != 1 || rs[0].Result != "restored" || rs[0].Generation != 2 {
		t.Fatalf("restore = %+v", rs)
	}
	if k := h.launcherKind("br-sysd"); k != "systemd" {
		t.Errorf("generation 2 launcher = %s, want systemd", k)
	}
	h.ok("stop", "br-sysd")
	h.waitState("br-sysd", "exited", 15*time.Second)
}

// orphanWrapper is a profile command that leaves a marked grandchild behind
// (same process group, ignoring SIGTERM) and then fails as an untrusted
// folder would.
func orphanWrapper(t *testing.T) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "orphaning-agent")
	body := "#!/bin/sh\nFAKE_ROLE=grandchild " + fakeBin + " >/dev/null 2>&1 &\nFAKE_UNTRUSTED=1 exec " + fakeBin + " \"$@\"\n"
	if err := os.WriteFile(w, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestRestoreFailedOrphans: a relaunch that failed but left a marked process
// behind shows it in inspect; plain rm refuses, stop and rm --force clean up.
func TestRestoreFailedOrphans(t *testing.T) {
	h := newBridgeHarness(t)
	full := fmt.Sprintf(fakeBridgeProfile, fakeBin)
	for _, n := range []string{"rf-stop", "rf-force"} {
		h.startOnReboot(n, "--grace=300ms")
		h.simulateReboot(n)
	}
	h.writeProfile("fakebridge", strings.Replace(full, fmt.Sprintf("%q", fakeBin), fmt.Sprintf("%q", orphanWrapper(t)), 1))
	rs := h.restore(1)
	if len(rs) != 2 || rs[0].Reason != "untrusted_workspace" || rs[1].Reason != "untrusted_workspace" {
		t.Fatalf("restore = %+v", rs)
	}
	for _, n := range []string{"rf-stop", "rf-force"} {
		if v := h.inspect(n); v.State != "restore-failed" || v.Orphans != 1 {
			t.Fatalf("%s: state %s, orphans %d; want restore-failed with 1", n, v.State, v.Orphans)
		}
		if _, errOut, code := h.cmd("rm", n); code != 1 || !strings.Contains(errOut, "orphans: 1") {
			t.Errorf("rm %s: exit %d, %q", n, code, errOut)
		}
	}
	h.ok("stop", "rf-stop")
	if v := h.inspect("rf-stop"); v.State != "restore-failed" || v.Orphans != 0 {
		t.Errorf("after stop: state %s, orphans %d", v.State, v.Orphans)
	}
	h.ok("rm", "rf-stop")
	h.ok("rm", "--force", "rf-force")
	if out := h.ok("ps", "--all", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("left behind: %s", out)
	}
}

// TestRestoreStoppedWhileWaiting: a relaunched bridge stopped while restore
// waits for its URL is skipped as stopped and stays exited.
func TestRestoreStoppedWhileWaiting(t *testing.T) {
	h := newBridgeHarness(t)
	h.writeConfig("remote_wait = \"500ms\"\n") // the URL never comes; do not wait long for it here
	h.ok("run", "fakebridge", "--remote", "--restart", "on-reboot", "--cwd", h.work, "--name", "br-stop",
		"--env", "FAKE_BRIDGE=1", "--env", "FAKE_BRIDGE_URL_AFTER=1h", "--grace", "2s")
	h.simulateReboot("br-stop")
	h.writeConfig("remote_wait = \"20s\"\n") // restore is still waiting when stop comes
	done := make(chan string, 1)
	go func() {
		out, _, _ := h.run("restore", "--json")
		done <- out
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		v := h.restoreInspect("br-stop")
		if v.State == "running" && v.Status.Generation == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generation 2 never ran: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.ok("stop", "br-stop")
	var rs []restoreJSON
	if err := json.Unmarshal([]byte(<-done), &rs); err != nil || len(rs) != 1 {
		t.Fatalf("restore output: %v %+v", err, rs)
	}
	if r := rs[0]; r.Result != "skipped" || r.Reason != "stopped" || r.Generation != 2 {
		t.Errorf("restore = %+v", r)
	}
	if v := h.restoreInspect("br-stop"); v.State != "exited" || v.Status.RestoreReason != "" {
		t.Errorf("after stop: %+v", v)
	}
}

// TestRmRefusesDuringRestore: rm never removes a session whose restore lock
// is held.
func TestRmRefusesDuringRestore(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-rm")
	h.simulateReboot("br-rm")
	f, err := os.OpenFile(filepath.Join(h.sessDir("br-rm"), "restore.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"rm", "br-rm"}, {"rm", "--force", "br-rm"}} {
		if _, errOut, code := h.cmd(args...); code != 1 || !strings.Contains(errOut, "a restore is in progress") {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	h.ok("rm", "br-rm")
}

// readPid waits for a pid file and returns its pid; the process is killed at
// cleanup.
func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			var pid int
			fmt.Sscan(string(b), &pid)
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRestoreFailedSetsidOrphan: a failed relaunch leaves a marked child that
// moved out of the process group (setsid). restore-failed sessions are
// scanned in every group, so it is counted, rm refuses, stop kills it, and a
// named restore then relaunches.
func TestRestoreFailedSetsidOrphan(t *testing.T) {
	h := newBridgeHarness(t)
	full := fmt.Sprintf(fakeBridgeProfile, fakeBin)
	h.startOnReboot("rf-setsid", "--grace=300ms")
	h.simulateReboot("rf-setsid")
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	wrapper := filepath.Join(t.TempDir(), "detaching-agent")
	body := "#!/bin/sh\nFAKE_SETSID_CHILD=" + pidfile + " FAKE_UNTRUSTED=1 exec " + fakeBin + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	h.writeProfile("fakebridge", strings.Replace(full, fmt.Sprintf("%q", fakeBin), fmt.Sprintf("%q", wrapper), 1))
	if rs := h.restore(1); len(rs) != 1 || rs[0].Reason != "untrusted_workspace" {
		t.Fatalf("restore = %+v", rs)
	}
	pid := readPid(t, pidfile)
	if v := h.inspect("rf-setsid"); v.State != "restore-failed" || v.Orphans != 1 {
		t.Fatalf("state %s, orphans %d; want restore-failed with 1", v.State, v.Orphans)
	}
	if _, errOut, code := h.cmd("rm", "rf-setsid"); code != 1 || !strings.Contains(errOut, "orphans: 1") {
		t.Errorf("rm: exit %d, %q", code, errOut)
	}
	h.ok("stop", "rf-setsid")
	waitGone(t, pid)
	if v := h.inspect("rf-setsid"); v.Orphans != 0 {
		t.Errorf("orphans after stop: %d", v.Orphans)
	}
	h.writeProfile("fakebridge", full)
	if rs := h.restore(0, "rf-setsid"); len(rs) != 1 || rs[0].Result != "restored" || rs[0].Generation != 3 {
		t.Errorf("named restore = %+v", rs)
	}
}

// TestLostHeadlessKeepsDaemonizedChild: a headless session's agent that moved
// a child out of its process group (as a tmux server or dev server would)
// does not have it counted when the monitor dies, and stop leaves it alone.
func TestLostHeadlessKeepsDaemonizedChild(t *testing.T) {
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	h.start("hl-daemon", []string{"FAKE_SLEEP=1h", "FAKE_SETSID_CHILD=" + pidfile}, "--grace", "300ms")
	pid := readPid(t, pidfile)
	h.waitState("hl-daemon", "running", 10*time.Second)
	h.simulateReboot("hl-daemon")
	if v := h.inspect("hl-daemon"); v.State != "lost" || v.Orphans != 0 {
		t.Fatalf("state %s, orphans %d; want lost with 0", v.State, v.Orphans)
	}
	h.ok("stop", "hl-daemon")
	time.Sleep(500 * time.Millisecond)
	if !pidAlive(pid) {
		t.Error("stop killed a child the agent had moved out of its process group")
	}
	h.ok("rm", "hl-daemon")
	if !pidAlive(pid) {
		t.Error("rm killed it")
	}
}

// TestRestoreSessionRemovedWhileWaiting: a restore that waited for the lock
// and finds the session gone reports skipped/not_found and exits 0.
func TestRestoreSessionRemovedWhileWaiting(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-gone")
	h.simulateReboot("br-gone")
	f, err := os.OpenFile(filepath.Join(h.sessDir("br-gone"), "restore.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	type res struct {
		out  string
		code int
	}
	done := make(chan res, 1)
	go func() {
		out, _, code := h.run("restore", "--json", "br-gone")
		done <- res{out, code}
	}()
	time.Sleep(700 * time.Millisecond) // restore has opened the lock and is waiting
	if err := os.RemoveAll(h.sessDir("br-gone")); err != nil {
		t.Fatal(err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	r := <-done
	var rs []restoreJSON
	if err := json.Unmarshal([]byte(r.out), &rs); err != nil {
		t.Fatalf("%v: %q", err, r.out)
	}
	if r.code != 0 || len(rs) != 1 || rs[0].Result != "skipped" || rs[0].Reason != "not_found" {
		t.Errorf("exit %d, %+v", r.code, rs)
	}
}
