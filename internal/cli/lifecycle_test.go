package cli_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCompletesWithParserFields(t *testing.T) {
	h := newHarness(t)
	h.start("s1", nil)
	if _, _, code := h.cmd("wait", "s1", "--timeout", "10s"); code != 0 {
		t.Fatalf("wait exit %d", code)
	}
	v := h.inspect("s1")
	if v.State != "exited" || v.Status.EndedBy != "exit" || v.Status.ExitCode == nil || *v.Status.ExitCode != 0 {
		t.Fatalf("got %+v", v)
	}
	if v.Status.ConversationID != "fake-session-0001" || v.Status.FinalMessage != "pong" {
		t.Fatalf("parser fields: %+v", v.Status)
	}
	if !strings.Contains(h.ok("logs", "s1"), `"result":"pong"`) {
		t.Fatal("logs missing result line")
	}
}

func TestSessionOutlivesCLI(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=2s"}) // the CLI has already exited here
	h.waitState("s1", "running", 5*time.Second)
	if !strings.Contains(h.ok("ps"), "s1") {
		t.Fatal("ps does not list running session")
	}
	h.waitState("s1", "exited", 10*time.Second)
}

func TestDuplicateName(t *testing.T) {
	h := newHarness(t)
	h.start("dup", []string{"FAKE_SLEEP=2s"})
	_, errOut, code := h.cmd("run", "fake", "--name", "dup", "--cwd", h.work, "--prompt", "x")
	if code != 1 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("duplicate: code %d, %s", code, errOut)
	}
}

func TestLaunchFailedBadCwd(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.cmd("run", "fake", "--name", "bad", "--cwd", "/nonexistent/dir", "--prompt", "x")
	if code != 1 || !strings.Contains(errOut, "launch failed") {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if v := h.inspect("bad"); v.Status.EndedBy != "launch-failed" || v.Status.Error == "" {
		t.Fatalf("got %+v", v.Status)
	}
	if _, _, code := h.cmd("wait", "bad"); code != 125 {
		t.Fatalf("wait on launch-failed: %d, want 125", code)
	}
}

func TestBinaryMissingCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("ghost", "schema_version = 1\nname = \"ghost\"\ncommand = \"/nonexistent/agent\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n")
	_, errOut, code := h.cmd("run", "ghost", "--name", "g", "--cwd", h.work, "--prompt", "x")
	if code != 1 || !strings.Contains(errOut, "not an absolute path to an executable") {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if _, _, code := h.cmd("inspect", "g"); code != 127 {
		t.Fatalf("session dir created for a failed pre-flight (inspect exit %d)", code)
	}
}

func TestNoSilentPermissionFallback(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("onlyskip", "schema_version = 1\nname = \"onlyskip\"\ncommand = \""+fakeBin+"\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nskip = []\n")
	_, errOut, code := h.cmd("run", "onlyskip", "--cwd", h.work, "--prompt", "x")
	if code != 1 || !strings.Contains(errOut, `permission level "auto"`) {
		t.Fatalf("code %d: %s", code, errOut)
	}
}

func TestStopIgnoringAgentAndGrandchild(t *testing.T) {
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_IGNORE_TERM=1", "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	h.waitState("s1", "running", 5*time.Second)
	gcPid := waitPid(t, pidfile)
	start := time.Now()
	h.ok("stop", "s1", "--grace", "1s") // per-call grace, shorter than the session's 10s default
	took := time.Since(start)
	v := h.inspect("s1")
	if v.Status.EndedBy != "stop" || v.Status.Signal != "SIGKILL" {
		t.Fatalf("got %+v", v.Status)
	}
	if took < time.Second {
		t.Fatal("grace not honoured")
	}
	if took > 8*time.Second {
		t.Fatalf("stop took %s: the per-call --grace was ignored in favour of the session's", took)
	}
	if pidAlive(gcPid) {
		t.Fatalf("grandchild %d survived stop", gcPid)
	}
}

func TestExternalSigtermIsSignalNotStop(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=60s"})
	v := h.waitState("s1", "running", 5*time.Second)
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGTERM)
	v = h.waitState("s1", "exited", 15*time.Second)
	if v.Status.EndedBy != "signal" {
		t.Fatalf("ended_by %q, want signal", v.Status.EndedBy)
	}
}

func TestMaxRuntime(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=60s"}, "--max-runtime", "1s", "--grace", "1s")
	_, _, code := h.cmd("wait", "s1", "--timeout", "15s")
	v := h.inspect("s1")
	if v.Status.EndedBy != "limit" || v.Status.Signal != "SIGTERM" || code != 128+15 {
		t.Fatalf("code %d, %+v", code, v.Status)
	}
}

func TestMonitorKilledLeavesLostWithOrphans(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pdeathsig behaviour is Linux-specific")
	}
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	v := h.waitState("s1", "running", 5*time.Second)
	waitPid(t, pidfile)
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	v = h.waitState("s1", "lost", 5*time.Second)
	if v.Orphans < 1 {
		t.Fatalf("orphans = %d, want the grandchild", v.Orphans)
	}
	if _, _, code := h.cmd("rm", "s1"); code != 1 {
		t.Fatal("rm without --force removed a session with orphans")
	}
	h.ok("stop", "s1", "--grace", "1s")
	v = h.inspect("s1")
	if v.State != "exited" || v.Status.EndedBy != "stop" || v.Orphans != 0 {
		t.Fatalf("after stop: %+v", v)
	}
	gc, _ := os.ReadFile(pidfile)
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(gc))); pid > 0 && pidAlive(pid) {
		t.Fatalf("grandchild %d survived", pid)
	}
}

func TestWaitCodes(t *testing.T) {
	h := newHarness(t)
	h.start("slow", []string{"FAKE_SLEEP=10s"})
	if _, _, code := h.cmd("wait", "slow", "--timeout", "300ms"); code != 124 {
		t.Fatalf("timeout code %d", code)
	}
	h.start("three", []string{"FAKE_EXIT=3"})
	if _, _, code := h.cmd("wait", "three", "--timeout", "10s"); code != 3 {
		t.Fatalf("exit code %d", code)
	}
	if _, _, code := h.cmd("wait", "nope"); code != 127 {
		t.Fatalf("unknown session code %d", code)
	}
	h.start("crash", []string{"FAKE_CRASH=1"})
	if _, _, code := h.cmd("wait", "crash", "--timeout", "10s"); code != 128+9 {
		t.Fatalf("crash code %d, want 137", code)
	}
	// Exit 0 with agent_error (pi's json mode on a model error) is abnormal.
	h.start("agenterr", []string{"FAKE_IS_ERROR=1", "FAKE_EXIT=0"})
	_, errOut, code := h.cmd("wait", "agenterr", "--timeout", "10s")
	if code != 125 || !strings.Contains(errOut, "agent reported an error") {
		t.Fatalf("agent error code %d, want 125 (%s)", code, errOut)
	}
	// A non-zero exit keeps its own code even with agent_error.
	h.start("agenterr3", []string{"FAKE_IS_ERROR=1", "FAKE_EXIT=3"})
	if _, _, code := h.cmd("wait", "agenterr3", "--timeout", "10s"); code != 3 {
		t.Fatalf("agent error with exit 3: code %d", code)
	}
}

func TestRmRefusesAliveUnlessForce(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=60s"})
	if _, _, code := h.cmd("rm", "s1"); code != 1 {
		t.Fatal("rm removed a running session")
	}
	h.ok("rm", "--force", "s1")
	if _, _, code := h.cmd("inspect", "s1"); code != 127 {
		t.Fatal("session still present")
	}
}

func TestEnvironmentIsExplicit(t *testing.T) {
	h := newHarness(t)
	h.extraEnv = []string{"LEAKY=from-caller"}
	h.start("leak", []string{"FAKE_REPLY_ENV=LEAKY"})
	h.ok("wait", "leak", "--timeout", "10s")
	if v := h.inspect("leak"); v.Status.FinalMessage != "" {
		t.Fatalf("caller env leaked: %q", v.Status.FinalMessage)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envFile, []byte("SECRET=from-file\n"), 0o600)
	h.start("file", []string{"FAKE_REPLY_ENV=SECRET"}, "--env-file", envFile)
	h.ok("wait", "file", "--timeout", "10s")
	if v := h.inspect("file"); v.Status.FinalMessage != "from-file" {
		t.Fatalf("env-file not applied: %q", v.Status.FinalMessage)
	}
	sess, _ := os.ReadFile(filepath.Join(h.state, "sessions", "file", "session.json"))
	if strings.Contains(string(sess), "from-file") {
		t.Fatal("env-file contents recorded in session.json")
	}
	h.start("marker", []string{"FAKE_REPLY_ENV=FASTENER_SESSION"})
	h.ok("wait", "marker", "--timeout", "10s")
	if v := h.inspect("marker"); !strings.HasPrefix(v.Status.FinalMessage, "marker:1:") {
		t.Fatalf("marker = %q", v.Status.FinalMessage)
	}
}

func TestPlanRecordsWhatRan(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=1s"})
	original, _ := os.ReadFile(filepath.Join(h.config, "backends", "fake.toml"))
	h.writeProfile("fake", string(original)+"\n# edited after launch\n")
	h.ok("wait", "s1", "--timeout", "10s")
	sum := sha256.Sum256(original)
	if v := h.inspect("s1"); v.Plan == nil || v.Plan.ProfileSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("plan sha does not match the profile that ran: %+v", v.Plan)
	}
}

// waitPid waits up to 3s for pidfile to hold a pid and fails if it never does.
func waitPid(t *testing.T, pidfile string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(pidfile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pidfile %s never held a pid", pidfile)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunJSONFailureNamesSession(t *testing.T) {
	h := newHarness(t)
	out, errOut, code := h.cmd("run", "fake", "--name", "badj", "--cwd", "/nonexistent/dir", "--prompt", "x", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	var v struct {
		Name   string `json:"name"`
		Status struct {
			EndedBy string `json:"ended_by"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if v.Name != "badj" || v.Status.EndedBy != "launch-failed" {
		t.Fatalf("got %+v", v)
	}
	if !strings.Contains(errOut, "/nonexistent/dir") || !strings.Contains(errOut, "badj") {
		t.Fatalf("stderr should mention the cwd and session: %s", errOut)
	}
}

func TestPromptFile(t *testing.T) {
	h := newHarness(t)
	pf := filepath.Join(t.TempDir(), "p.txt")
	os.WriteFile(pf, []byte("from a file"), 0o600)
	h.ok("run", "fake", "--name", "pf", "--cwd", h.work, "--prompt-file", pf)
	h.ok("wait", "pf", "--timeout", "10s")
	b, err := os.ReadFile(filepath.Join(h.state, "sessions", "pf", "prompt.txt"))
	if err != nil || string(b) != "from a file" {
		t.Fatalf("prompt.txt = %q, %v", b, err)
	}
	var v struct {
		Session struct {
			PromptSource string `json:"prompt_source"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(h.ok("inspect", "pf", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Session.PromptSource != pf {
		t.Fatalf("prompt_source = %q, want %q", v.Session.PromptSource, pf)
	}
}

func TestRmForceCorruptStatus(t *testing.T) {
	h := newHarness(t)
	h.start("c1", nil)
	h.ok("wait", "c1", "--timeout", "10s")
	os.WriteFile(filepath.Join(h.state, "sessions", "c1", "status.json"), []byte("{not json"), 0o600)
	if _, errOut, code := h.cmd("rm", "c1"); code != 1 || !strings.Contains(errOut, "--force") {
		t.Fatalf("rm without --force: %d %s", code, errOut)
	}
	h.ok("rm", "--force", "c1")
	if _, _, code := h.cmd("inspect", "c1"); code != 127 {
		t.Fatal("corrupt session still present")
	}
}

func TestBadFlagsAreUsageErrors(t *testing.T) {
	h := newHarness(t)
	base := []string{"run", "fake", "--cwd", h.work, "--prompt", "x"}
	for _, extra := range [][]string{{"--env", "BAD-KEY=1"}, {"--env", "NOEQUALS"}, {"--grace", "-1s"}, {"--max-runtime", "-1s"}} {
		if _, errOut, code := h.cmd(append(append([]string{}, base...), extra...)...); code != 2 {
			t.Fatalf("%v: exit %d, want 2 (%s)", extra, code, errOut)
		}
	}
}

func TestLogsFollowTerminates(t *testing.T) {
	h := newHarness(t)
	h.start("lf", []string{"FAKE_SLEEP=300ms"})
	done := make(chan string, 1)
	go func() {
		out, _, code := h.cmd("logs", "-f", "lf")
		if code != 0 {
			out = "exit " + strconv.Itoa(code)
		}
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, `"result":"pong"`) {
			t.Fatalf("logs -f missed the result line: %s", out)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("logs -f did not terminate")
	}
}

func TestHelpExitsZero(t *testing.T) {
	h := newHarness(t)
	for _, c := range []string{"inspect", "run", "ps"} {
		if _, _, code := h.cmd(c, "-h"); code != 0 {
			t.Fatalf("%s -h exit %d, want 0", c, code)
		}
	}
	out, _, code := h.cmd("help")
	if code != 0 || !strings.Contains(out, "https://github.com/ytjohn/fastener") {
		t.Fatalf("help: exit %d, missing repo link:\n%s", code, out)
	}
}

func TestRmForceCleansLostWithOrphans(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pdeathsig behaviour is Linux-specific")
	}
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_SLEEP=60s", "FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile}, "--grace", "1s")
	v := h.waitState("s1", "running", 5*time.Second)
	waitPid(t, pidfile)
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	if v = h.waitState("s1", "lost", 5*time.Second); v.Orphans < 1 {
		t.Fatalf("orphans = %d", v.Orphans)
	}
	h.ok("rm", "--force", "s1")
	if _, _, code := h.cmd("inspect", "s1"); code != 127 {
		t.Fatal("session not removed")
	}
	gc, _ := os.ReadFile(pidfile)
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(gc))); pid > 0 && pidAlive(pid) {
		t.Fatalf("grandchild %d survived rm --force", pid)
	}
}

func TestWaitOnLostSessionExits125(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=60s"})
	v := h.waitState("s1", "running", 5*time.Second)
	syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	h.waitState("s1", "lost", 5*time.Second)
	if _, _, code := h.cmd("wait", "s1", "--timeout", "5s"); code != 125 {
		t.Fatalf("wait on lost session: exit %d, want 125", code)
	}
}

func TestRunJSONWithFollowIsUsageError(t *testing.T) {
	h := newHarness(t)
	if _, _, code := h.cmd("run", "fake", "--cwd", h.work, "--prompt", "x", "--json", "--follow"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestHandshakeTimeoutStopsTheSession(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.config, "config.toml"), []byte("handshake_timeout = \"1ns\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "fake", "--name", "slowstart", "--cwd", h.work, "--prompt", "x", "--env", "FAKE_SLEEP=60s"}
	_, errOut, code := h.cmd(args...)
	if code == 0 {
		t.Skip("monitor reached running before the 1ns handshake deadline; nothing to assert")
	}
	if !strings.Contains(errOut, "slowstart") {
		t.Errorf("error should name the session: %s", errOut)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		v := h.inspect("slowstart")
		if v.State == "exited" {
			if v.Status.EndedBy == "launch-failed" || v.Status.EndedBy == "stop" {
				return
			}
			t.Fatalf("ended_by = %q", v.Status.EndedBy)
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still %s after handshake timeout; prompt left running", v.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestExitedSessionTracksAndCleansDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process scan test is Linux-specific")
	}
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	if _, _, code := h.cmd("wait", "s1", "--timeout", "10s"); code != 0 {
		t.Fatalf("wait exit %d", code)
	}
	gcPid := waitPid(t, pidfile)
	v := h.inspect("s1")
	if v.State != "exited" || v.Orphans < 1 {
		t.Fatalf("state %s orphans %d, want exited with the grandchild tracked", v.State, v.Orphans)
	}
	if _, _, code := h.cmd("rm", "s1"); code != 1 {
		t.Fatal("rm without --force removed an exited session with orphans")
	}
	h.ok("stop", "s1", "--grace", "1s")
	if pidAlive(gcPid) {
		t.Fatalf("grandchild %d survived stop", gcPid)
	}
	v = h.inspect("s1")
	if v.State != "exited" || v.Status.EndedBy != "exit" || v.Orphans != 0 {
		t.Fatalf("stop re-finalized or left orphans: %+v", v)
	}
	h.ok("rm", "s1")
}

func TestRmForceKillsDescendantsOfExitedSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process scan test is Linux-specific")
	}
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("s1", []string{"FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile}, "--grace", "1s")
	h.cmd("wait", "s1", "--timeout", "10s")
	gcPid := waitPid(t, pidfile)
	h.ok("rm", "--force", "s1")
	if pidAlive(gcPid) {
		t.Fatalf("grandchild %d survived rm --force", gcPid)
	}
}

func TestRunFollowReportsAgentOutcome(t *testing.T) {
	h := newHarness(t)
	run := func(name string, env ...string) (string, string, int) {
		args := []string{"run", "fake", "--name", name, "--cwd", h.work, "--prompt", "hello", "--follow"}
		for _, e := range env {
			args = append(args, "--env", e)
		}
		return h.cmd(args...)
	}
	out, errOut, code := run("f3", "FAKE_EXIT=3")
	if code != 3 || !strings.Contains(out, `"result":"pong"`) {
		t.Fatalf("exit 3 agent: code %d, want 3\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if _, errOut, code = run("f0"); code != 0 {
		t.Fatalf("exit 0 agent: code %d (%s)", code, errOut)
	}
	_, errOut, code = run("fe", "FAKE_IS_ERROR=1", "FAKE_EXIT=0")
	if code != 125 || !strings.Contains(errOut, "agent reported an error") {
		t.Fatalf("agent_error: code %d, want 125 (%s)", code, errOut)
	}
	if _, _, code = run("fc", "FAKE_CRASH=1"); code != 128+9 {
		t.Fatalf("crash: code %d, want 137", code)
	}
}

func TestWaitSessionRemovedMidWaitExits125(t *testing.T) {
	h := newHarness(t)
	h.start("rmw", []string{"FAKE_SLEEP=30s"})
	v := h.waitState("rmw", "running", 10*time.Second)
	t.Cleanup(func() { killGroup(v.Status.PGID) })
	type result struct {
		errOut string
		code   int
	}
	done := make(chan result, 1)
	go func() {
		_, e, c := h.cmd("wait", "rmw", "--timeout", "30s")
		done <- result{e, c}
	}()
	time.Sleep(500 * time.Millisecond) // let wait load the running session at least once
	h.removeSessionDir("rmw")
	select {
	case r := <-done:
		if r.code != 125 || !strings.Contains(r.errOut, "session rmw was removed while waiting") {
			t.Fatalf("code %d, want 125 with removal message: %s", r.code, r.errOut)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait did not return after the session was removed")
	}
	if _, _, code := h.cmd("wait", "rmw"); code != 127 {
		t.Fatalf("never-existed/now-gone session: code %d, want 127", code)
	}
}

// removeSessionDir drops a session's directory under a running agent.
// `rm --force` stops the agent first, so a poll could legitimately see the
// exited state before the directory goes; this tests the removal path alone.
func (h *harness) removeSessionDir(name string) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.state, "sessions", name)); err != nil {
		h.t.Fatal(err)
	}
}

// pollRunningPGID waits for a session that may not exist yet to be running.
func (h *harness) pollRunningPGID(name string) int {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, _, code := h.cmd("inspect", name, "--json"); code == 0 {
			var v inspectJSON
			if json.Unmarshal([]byte(out), &v) == nil && v.State == "running" && v.Status.PGID > 0 {
				return v.Status.PGID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("%s never reached running", name)
	return 0
}

func killGroup(pgid int) {
	if pgid > 1 {
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

func TestRunFollowSessionRemovedExits125(t *testing.T) {
	h := newHarness(t)
	c := exec.Command(fastenerBin, "run", "fake", "--name", "rmf", "--cwd", h.work, "--prompt", "hello",
		"--env", "FAKE_SLEEP=30s", "--follow")
	c.Env = append(os.Environ(), "FASTENER_STATE_DIR="+h.state, "FASTENER_CONFIG_DIR="+h.config, "TMPDIR="+h.work)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- c.Wait() }()
	pgid := h.pollRunningPGID("rmf")
	t.Cleanup(func() { killGroup(pgid) })
	time.Sleep(500 * time.Millisecond)
	h.removeSessionDir("rmf")
	select {
	case err := <-waited:
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 125 || !strings.Contains(errb.String(), "session rmf was removed while following") {
			t.Fatalf("err %v, stderr %q; want 125 with removal message", err, errb.String())
		}
	case <-time.After(15 * time.Second):
		c.Process.Kill()
		t.Fatal("run --follow did not return after the session was removed")
	}
}

func TestPromptFileStdin(t *testing.T) {
	h := newHarness(t)
	c := exec.Command(fastenerBin, "run", "fake", "--name", "pin", "--cwd", h.work, "--prompt-file", "-")
	c.Env = append(os.Environ(), "FASTENER_STATE_DIR="+h.state, "FASTENER_CONFIG_DIR="+h.config, "TMPDIR="+h.work)
	c.Stdin = strings.NewReader("prompt from stdin")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	h.ok("wait", "pin", "--timeout", "10s")
	b, err := os.ReadFile(filepath.Join(h.state, "sessions", "pin", "prompt.txt"))
	if err != nil || string(b) != "prompt from stdin" {
		t.Fatalf("prompt.txt = %q, %v", b, err)
	}
	var v struct {
		Session struct {
			PromptSource string `json:"prompt_source"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(h.ok("inspect", "pin", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Session.PromptSource != "stdin" {
		t.Fatalf("prompt_source = %q, want stdin", v.Session.PromptSource)
	}
}

func TestPromptSizeCap(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("a", 4<<20+1)
	pf := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(pf, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := h.cmd("run", "fake", "--name", "bigf", "--cwd", h.work, "--prompt-file", pf)
	if code != 2 || !strings.Contains(errOut, "4 MiB") {
		t.Fatalf("oversize file: code %d, want 2 naming the cap: %s", code, errOut)
	}
	c := exec.Command(fastenerBin, "run", "fake", "--name", "bigs", "--cwd", h.work, "--prompt-file", "-")
	c.Env = append(os.Environ(), "FASTENER_STATE_DIR="+h.state, "FASTENER_CONFIG_DIR="+h.config, "TMPDIR="+h.work)
	c.Stdin = strings.NewReader(big)
	out, err := c.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "4 MiB") {
		t.Fatalf("oversize stdin: %v\n%s", err, out)
	}
	// Exactly the cap is fine.
	if err := os.WriteFile(pf, []byte(big[:4<<20]), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ok("run", "fake", "--name", "capok", "--cwd", h.work, "--prompt-file", pf)
	for _, n := range []string{"bigf", "bigs"} {
		if _, _, code := h.cmd("inspect", n); code != 127 {
			t.Errorf("%s: a session was created for a rejected prompt", n)
		}
	}
}

func TestPromptFileReadsFIFO(t *testing.T) {
	h := newHarness(t)
	fifo := filepath.Join(t.TempDir(), "p.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	go func() { // stands in for process substitution: <(cmd)
		if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			f.WriteString("prompt from a pipe")
			f.Close()
		}
	}()
	h.ok("run", "fake", "--name", "pipe", "--cwd", h.work, "--prompt-file", fifo)
	h.ok("wait", "pipe", "--timeout", "10s")
	b, err := os.ReadFile(filepath.Join(h.state, "sessions", "pipe", "prompt.txt"))
	if err != nil || string(b) != "prompt from a pipe" {
		t.Fatalf("prompt.txt = %q, %v", b, err)
	}
}

func TestPromptFileRejectsDevicesAndDirs(t *testing.T) {
	h := newHarness(t)
	for name, p := range map[string]string{"zero": "/dev/zero", "dir": t.TempDir()} {
		done := make(chan struct{})
		var errOut string
		var code int
		go func() {
			defer close(done)
			_, errOut, code = h.cmd("run", "fake", "--name", "nr-"+name, "--cwd", h.work, "--prompt-file", p)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: run hung", name)
		}
		if code != 2 || !strings.Contains(errOut, "not a regular file or pipe") {
			t.Errorf("%s: code %d, want 2 with a not-a-regular-file error: %s", name, code, errOut)
		}
	}
}
