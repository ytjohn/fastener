package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
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
	if v.State != "exited" || v.Status.EndedBy != "exit" || *v.Status.ExitCode != 0 {
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
	h.waitState("s1", "running", 2*time.Second)
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
	h.waitState("s1", "running", 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	gc, _ := os.ReadFile(pidfile)
	gcPid, _ := strconv.Atoi(strings.TrimSpace(string(gc)))
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
	if gcPid > 0 && pidAlive(gcPid) {
		t.Fatalf("grandchild %d survived stop", gcPid)
	}
}

func TestExternalSigtermIsSignalNotStop(t *testing.T) {
	h := newHarness(t)
	h.start("s1", []string{"FAKE_SLEEP=60s"})
	v := h.waitState("s1", "running", 2*time.Second)
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
	v := h.waitState("s1", "running", 2*time.Second)
	time.Sleep(200 * time.Millisecond)
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
