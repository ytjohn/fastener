package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
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
