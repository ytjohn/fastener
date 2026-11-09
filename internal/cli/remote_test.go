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
