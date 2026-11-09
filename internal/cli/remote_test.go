package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newBridgeHarness is a harness with the fakebridge profile installed.
func newBridgeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.writeProfile("fakebridge", fmt.Sprintf(fakeBridgeProfile, fakeBin))
	return h
}

type remoteSessionJSON struct {
	Kind        string `json:"kind"`
	Remote      bool   `json:"remote"`
	MaxRuntime  string `json:"max_runtime"`
	Permissions string `json:"permissions"`
}

type remoteJSON struct {
	Name    string            `json:"name"`
	State   string            `json:"state"`
	Reason  string            `json:"reason"`
	Folder  string            `json:"folder"`
	Session remoteSessionJSON `json:"session"`
	Status  struct {
		EndedBy        string   `json:"ended_by"`
		RemoteURL      string   `json:"remote_url"`
		RemoteSessions []string `json:"remote_sessions"`
	} `json:"status"`
	Plan struct {
		Argv         []string `json:"argv"`
		PromptMode   string   `json:"prompt_mode"`
		Parser       string   `json:"parser"`
		DedupeFrames bool     `json:"dedupe_frames"`
	} `json:"plan"`
}

func (h *harness) remoteInspect(name string) remoteJSON {
	h.t.Helper()
	var v remoteJSON
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

func TestRunRemoteUsageErrors(t *testing.T) {
	h := newBridgeHarness(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"run", "fakebridge", "--remote", "--cwd", h.work, "--prompt", "hi"}, "cannot be used with --remote"},
		{[]string{"run", "fakebridge", "--remote", "--cwd", h.work, "--prompt-file", "/dev/null"}, "cannot be used with --remote"},
		{[]string{"run", "fakebridge", "--remote", "--cwd", h.work, "--model", "m"}, "--model cannot be used with --remote"},
		{[]string{"run", "fake", "--remote", "--cwd", h.work}, "backend fake has no [remote] section"},
		{[]string{"run", "fakebridge", "--remote"}, "--cwd is required"},
	}
	for _, c := range cases {
		_, errOut, code := h.cmd(c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, stderr %q; want exit 2 with %q", c.args, code, errOut, c.want)
		}
	}
	// Nothing was created.
	if out := h.ok("ps", "--all", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("sessions left behind: %s", out)
	}
}

func TestRunRemoteSessionRecord(t *testing.T) {
	h := newBridgeHarness(t)
	h.ok("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-1",
		"--permissions", "skip", "--env", "FAKE_BRIDGE=1")
	v := h.remoteInspect("br-1")
	if v.Session.Kind != "remote" || !v.Session.Remote || v.Session.MaxRuntime != "" {
		t.Errorf("session = %+v", v.Session)
	}
	argv := strings.Join(v.Plan.Argv[1:], " ")
	if argv != "bridge --name br-1 --permission-mode bypassPermissions" {
		t.Errorf("argv = %q", argv)
	}
	if v.Plan.PromptMode != "none" || v.Plan.Parser != "claude-remote" {
		t.Errorf("plan = %+v", v.Plan)
	}
	if _, err := os.Stat(filepath.Join(h.state, "sessions", "br-1", "prompt.txt")); !os.IsNotExist(err) {
		t.Errorf("prompt.txt written for a bridge: %v", err)
	}
	h.ok("stop", "br-1")
	if v := h.waitState("br-1", "exited", 10*time.Second); v.Status.EndedBy != "stop" {
		t.Errorf("ended_by = %q, want stop", v.Status.EndedBy)
	}
}

func TestRunRemotePrintsURL(t *testing.T) {
	h := newBridgeHarness(t)
	const url = "https://claude.ai/code?environment=env_01FAKEBRIDGEFAKEBRIDGE01"
	const sess = "https://claude.ai/code/session_01FAKEBRIDGESESSION01"
	out := h.ok("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-url",
		"--env", "FAKE_BRIDGE=1", "--env", "FAKE_BRIDGE_SESSION_AFTER=0s")
	if out != "br-url  "+url {
		t.Errorf("run printed %q, want %q", out, "br-url  "+url)
	}
	// The session link appears in a later frame, flushed within about 1s.
	deadline := time.Now().Add(5 * time.Second)
	var v remoteJSON
	for {
		v = h.remoteInspect("br-url")
		if len(v.Status.RemoteSessions) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if v.State != "running" || v.Status.RemoteURL != url || len(v.Status.RemoteSessions) != 1 || v.Status.RemoteSessions[0] != sess {
		t.Errorf("inspect = %+v", v)
	}
	human := h.ok("inspect", "br-url")
	for _, want := range []string{"remote_url", url, "remote_session", sess} {
		if !strings.Contains(human, want) {
			t.Errorf("inspect output lacks %q:\n%s", want, human)
		}
	}
	var rows []struct {
		Name      string `json:"name"`
		RemoteURL string `json:"remote_url"`
	}
	if err := json.Unmarshal([]byte(h.ok("ps", "--json")), &rows); err != nil || len(rows) != 1 || rows[0].RemoteURL != url {
		t.Errorf("ps --json = %+v, %v", rows, err)
	}
	h.ok("stop", "br-url")
	if v := h.waitState("br-url", "exited", 10*time.Second); v.Status.EndedBy != "stop" {
		t.Errorf("ended_by = %q, want stop", v.Status.EndedBy)
	}
}

func TestRunRemoteJSON(t *testing.T) {
	h := newBridgeHarness(t)
	out := h.ok("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-json", "--json", "--env", "FAKE_BRIDGE=1")
	var v remoteJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if v.Name != "br-json" || v.State != "running" || v.Reason != "" ||
		v.Status.RemoteURL != "https://claude.ai/code?environment=env_01FAKEBRIDGEFAKEBRIDGE01" {
		t.Errorf("run --json = %+v", v)
	}
}

func TestRunRemoteWaitTimeout(t *testing.T) {
	h := newBridgeHarness(t)
	h.writeConfig("remote_wait = \"700ms\"\n")
	start := time.Now()
	out, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-slow",
		"--env", "FAKE_BRIDGE=1", "--env", "FAKE_BRIDGE_URL_AFTER=1h")
	if code != 0 || strings.TrimSpace(out) != "br-slow" || !strings.Contains(errOut, "no URL yet after 700ms") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if el := time.Since(start); el < 700*time.Millisecond || el > 8*time.Second {
		t.Errorf("run took %s", el)
	}
	if v := h.remoteInspect("br-slow"); v.State != "running" || v.Status.RemoteURL != "" {
		t.Errorf("inspect = %+v", v)
	}
}

func TestRunRemoteUntrustedWorkspace(t *testing.T) {
	h := newBridgeHarness(t)
	dir := t.TempDir()
	want := dir + " is not a trusted workspace for fakeagent bridge; authorize it with: trust-it " + dir
	_, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", dir, "--name", "br-untrusted", "--env", "FAKE_UNTRUSTED=1")
	if code != 1 || strings.TrimSpace(errOut) != "fastener: "+want {
		t.Errorf("exit %d, stderr %q; want exit 1 with %q", code, errOut, want)
	}
	h.ok("rm", "br-untrusted")
	out, _, code := h.cmd("run", "fakebridge", "--remote", "--cwd", dir, "--name", "br-untrusted-json", "--json", "--env", "FAKE_UNTRUSTED=1")
	var v remoteJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if code != 1 || v.Reason != "untrusted_workspace" || v.Folder != dir || v.State != "exited" {
		t.Errorf("exit %d, json %+v", code, v)
	}
}

func TestRunRemoteEndsEarly(t *testing.T) {
	h := newBridgeHarness(t)
	// Not a bridge: the fake agent's headless mode exits at once.
	out, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-exit", "--json",
		"--env", "FAKE_EXIT=3", "--env", "FAKE_STDERR=boom: no bridge here")
	var v remoteJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if code != 1 || v.Reason != "exited" || v.Folder != "" ||
		!strings.Contains(errOut, "ended before reporting its URL (state=exited, ended_by=exit, exit_code=3)") ||
		!strings.Contains(errOut, "stderr: boom: no bridge here") {
		t.Errorf("exit %d, reason %q, stderr %q", code, v.Reason, errOut)
	}
}

func TestRunRemoteOutputLogStaysSmall(t *testing.T) {
	h := newBridgeHarness(t)
	h.ok("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-log",
		"--env", "FAKE_BRIDGE=1", "--env", "FAKE_BRIDGE_FRAMES=10000", "--env", "FAKE_BRIDGE_SESSION_AFTER=0s")
	// Let the burst through and a few timed redraws on top.
	time.Sleep(500 * time.Millisecond)
	h.ok("stop", "br-log")
	h.waitState("br-log", "exited", 10*time.Second)
	b, err := os.ReadFile(filepath.Join(h.state, "sessions", "br-log", "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 4096 {
		t.Errorf("output.log is %d bytes after 10,000+ frames; want it small", len(b))
	}
	for _, want := range []string{"environment=env_01FAKEBRIDGEFAKEBRIDGE01", "session_01FAKEBRIDGESESSION01", "Environment preserved"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("output.log lacks %q:\n%q", want, b)
		}
	}
	if v := h.remoteInspect("br-log"); !v.Plan.DedupeFrames || len(v.Status.RemoteSessions) != 1 {
		t.Errorf("inspect = %+v", v)
	}
}

// TestRunRemoteUntrustedNoHint: without untrusted_hint the error names no
// command.
func TestRunRemoteUntrustedNoHint(t *testing.T) {
	h := newBridgeHarness(t)
	h.writeProfile("fakebridge", strings.Replace(fmt.Sprintf(fakeBridgeProfile, fakeBin), "untrusted_hint    = \"trust-it {dir}\"\n", "", 1))
	dir := t.TempDir()
	_, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", dir, "--env", "FAKE_UNTRUSTED=1")
	if want := "fastener: " + dir + " is not a trusted workspace for fakeagent bridge\n"; code != 1 || errOut != want {
		t.Errorf("exit %d, stderr %q; want exit 1 with %q", code, errOut, want)
	}
}

// TestRunRemoteUntrustedFalseNegative: stderr that does not match
// untrusted_pattern is an ordinary early end.
func TestRunRemoteUntrustedFalseNegative(t *testing.T) {
	h := newBridgeHarness(t)
	out, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", h.work, "--json",
		"--env", "FAKE_EXIT=1", "--env", "FAKE_STDERR=[bridge] notice: workspace is trusted; network unreachable")
	var v remoteJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if code != 1 || v.Reason != "exited" || v.Folder != "" || strings.Contains(errOut, "not a trusted workspace") {
		t.Errorf("exit %d, reason %q, folder %q, stderr %q", code, v.Reason, v.Folder, errOut)
	}
}

// TestRunRemoteEndedAfterURL: a bridge that printed its URL and then exited
// before run saw it is reported as ending after the URL.
func TestRunRemoteEndedAfterURL(t *testing.T) {
	h := newBridgeHarness(t)
	const url = "https://claude.ai/code?environment=env_01FAKEBRIDGEFAKEBRIDGE01"
	out, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", h.work, "--json",
		"--env", "FAKE_BRIDGE=1", "--env", "FAKE_BRIDGE_EXIT_AFTER=200ms")
	var v remoteJSON
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if code != 1 || v.Reason != "exited" || v.Status.RemoteURL != url ||
		!strings.Contains(errOut, "the bridge ended after reporting "+url+" (state=exited, ended_by=exit, exit_code=0)") {
		t.Errorf("exit %d, json %+v, stderr %q", code, v, errOut)
	}
}

func TestRunRemoteFollowIsUsageError(t *testing.T) {
	h := newBridgeHarness(t)
	_, errOut, code := h.cmd("run", "fakebridge", "--remote", "--cwd", h.work, "--follow", "--env", "FAKE_BRIDGE=1")
	if code != 2 || !strings.Contains(errOut, "--follow cannot be used with --remote") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if out := h.ok("ps", "--all", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("a session was created: %s", out)
	}
}
