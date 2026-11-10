package cli_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

// fakeInteractiveProfile is fakeagent with [interactive] sections: its pty
// mode is switched on through [interactive].env, and the remote variant
// looks for the URL Claude Code prints.
const fakeInteractiveProfile = `schema_version = 1
name    = "%s"
command = %q

[headless]
args   = []
prompt = "stdin"
parser = "claude-stream-json"

[options]
model = ["--model", "{value}"]

[permissions]
auto = ["--auto"]
skip = []

[interactive]
args   = ["--tui"]
prompt = %q
env    = ["FAKE_PTY=1", "FAKE_PTY_ARGV=1"]
%s`

const fakeInteractiveRemote = `
[interactive.remote]
args              = ["--remote-control", "{name}"]
url_pattern       = 'https://claude\.ai/code/session_[A-Za-z0-9_-]+'
untrusted_pattern = '(?is)quick\s*safety\s*check\b.{0,1000}?\byes,\s*i\s*trust\s*this\s*folder'
untrusted_hint    = "fastener authorize claude-folder-trust {dir}"
`

func newInteractiveHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.writeProfile("fakei", fmt.Sprintf(fakeInteractiveProfile, "fakei", fakeBin, "arg", fakeInteractiveRemote))
	h.writeProfile("fakenp", fmt.Sprintf(fakeInteractiveProfile, "fakenp", fakeBin, "none", ""))
	return h
}

type interactiveInspect struct {
	State   string `json:"state"`
	Session struct {
		Kind   string `json:"kind"`
		Remote bool   `json:"remote"`
	} `json:"session"`
	Plan struct {
		Argv []string `json:"argv"`
		PTY  *struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		} `json:"pty"`
		URLPattern string `json:"url_pattern"`
	} `json:"plan"`
	Status struct {
		RemoteURL string `json:"remote_url"`
		EndedBy   string `json:"ended_by"`
	} `json:"status"`
	Reason string `json:"reason"`
	Folder string `json:"folder"`
}

func inspectI(t *testing.T, h *harness, name string) interactiveInspect {
	t.Helper()
	var v interactiveInspect
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitLog(t *testing.T, h *harness, name, want string) string {
	t.Helper()
	var out string
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if out = h.ok("logs", name); strings.Contains(out, want) {
			return out
		}
	}
	t.Fatalf("logs %s never showed %q:\n%q", name, want, out)
	return ""
}

// TestRunInteractiveDetachedThenAttach: run --interactive starts the TUI
// detached at the requested size with the prompt as its argument; attach
// then reaches it.
func TestRunInteractiveDetachedThenAttach(t *testing.T) {
	h := newInteractiveHarness(t)
	if out := h.ok("run", "fakei", "--interactive", "--cwd", h.work, "--name", "i1", "--size", "33x101",
		"--prompt", "hello there", "--model", "m1"); out != "i1" {
		t.Fatalf("run printed %q", out)
	}
	v := inspectI(t, h, "i1")
	if v.State != session.StateRunning || v.Session.Kind != session.KindInteractive || v.Session.Remote ||
		v.Plan.PTY == nil || v.Plan.PTY.Rows != 33 || v.Plan.PTY.Cols != 101 || v.Plan.URLPattern != "" {
		t.Fatalf("inspect: %+v", v)
	}
	// [interactive].args, the permission level, the model, then -- and the prompt.
	logs := waitLog(t, h, "i1", "size=33x101")
	if !strings.Contains(logs, "argv=--tui|--auto|--model|m1|--|hello there") || !strings.Contains(logs, "tty=true ctty=true") {
		t.Fatalf("logs:\n%q", logs)
	}
	var rows []struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Interactive bool   `json:"interactive"`
	}
	if err := json.Unmarshal([]byte(h.ok("ps", "--json")), &rows); err != nil || len(rows) != 1 ||
		rows[0].Kind != session.KindInteractive || !rows[0].Interactive {
		t.Fatalf("ps --json: %v %+v", err, rows)
	}
	a := startAttach(t, h, "i1", "--quiet")
	a.waitOutput("winch=30x100")
	a.typeIn("ping\r")
	a.waitOutput("echo:ping")
	a.typeIn("\x1d")
	if code := a.wait(); code != 0 {
		t.Fatalf("detach exited %d", code)
	}
	a.termiosRestored()
}

// TestRunInteractiveAttachFlag: run --attach launches, then behaves as
// attach: the TUI gets this terminal's size, keys reach it, and its exit
// code is run's.
func TestRunInteractiveAttachFlag(t *testing.T) {
	h := newInteractiveHarness(t)
	a := startOnPty(t, h, "run", "fakei", "--interactive", "--attach", "--cwd", h.work, "--name", "i2")
	a.waitOutput("winch=30x100") // no --size: this terminal's (the redraw on attach reports it)
	if v := inspectI(t, h, "i2"); v.Plan.PTY == nil || v.Plan.PTY.Rows != 30 || v.Plan.PTY.Cols != 100 {
		t.Fatalf("initial size not taken from the terminal: %+v", v.Plan.PTY)
	}
	a.rawDuring()
	a.typeIn("hi\r")
	a.waitOutput("echo:hi")
	a.typeIn("exit 4\r")
	if code := a.wait(); code != 4 {
		t.Fatalf("run --attach exited %d:\n%q", code, a.output())
	}
	a.termiosRestored()
	if !strings.Contains(a.output(), "[fastener: session i2 exited 4]") {
		t.Fatalf("terminal:\n%q", a.output())
	}
}

// TestRunInteractiveRemoteURL: run --interactive --remote waits for
// url_pattern to match the terminal output (the URL drawn with cursor moves
// and an OSC 8 link, split across writes, nothing after it) and prints it.
func TestRunInteractiveRemoteURL(t *testing.T) {
	h := newInteractiveHarness(t)
	const url = "https://claude.ai/code/session_01FAKEPTYSESSION0001"
	out := h.ok("run", "fakei", "--interactive", "--remote", "--cwd", h.work, "--name", "r1", "--env", "FAKE_PTY_RUN=url")
	if out != "r1  "+url {
		t.Fatalf("run printed %q", out)
	}
	v := inspectI(t, h, "r1")
	if v.Session.Kind != session.KindInteractive || !v.Session.Remote || v.Status.RemoteURL != url || v.Plan.URLPattern == "" {
		t.Fatalf("inspect: %+v", v)
	}
	if logs := h.ok("logs", "r1"); !strings.Contains(logs, "argv=--tui|--remote-control|r1|--auto") {
		t.Fatalf("argv: %q", logs)
	}
	var rows []struct {
		RemoteURL string `json:"remote_url"`
	}
	if err := json.Unmarshal([]byte(h.ok("ps", "--json")), &rows); err != nil || len(rows) != 1 || rows[0].RemoteURL != url {
		t.Fatalf("ps --json: %v %+v", err, rows)
	}
}

// TestRunInteractiveRemoteUntrusted: a TUI showing the folder trust dialog
// is reported untrusted (with the hint) and stopped.
func TestRunInteractiveRemoteUntrusted(t *testing.T) {
	h := newInteractiveHarness(t)
	out, errOut, code := h.cmd("run", "fakei", "--interactive", "--remote", "--cwd", h.work, "--name", "u1",
		"--env", "FAKE_PTY_RUN=cat "+trustDialogFixture(t), "--json")
	if code != 1 || !strings.Contains(errOut, "is not a trusted workspace for fakeagent --remote-control") ||
		!strings.Contains(errOut, "fastener authorize claude-folder-trust "+h.work) {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var v interactiveInspect
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Reason != "untrusted_workspace" || v.Folder != h.work {
		t.Fatalf("--json: %v %s", err, out)
	}
	if got := inspectI(t, h, "u1"); got.State != session.StateExited || got.Status.EndedBy != session.EndedByStop {
		t.Fatalf("untrusted session left %+v", got)
	}
}

// TestRunInteractiveRefusals covers what run --interactive refuses.
func TestRunInteractiveRefusals(t *testing.T) {
	h := newInteractiveHarness(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "fake", "--interactive", "--cwd", h.work}, "has no [interactive] section"},
		{[]string{"run", "fakenp", "--interactive", "--remote", "--cwd", h.work}, "no [interactive.remote] section"},
		{[]string{"run", "fakenp", "--interactive", "--cwd", h.work, "--prompt", "hi"}, "takes no initial prompt"},
		{[]string{"run", "fakei", "--interactive", "--cwd", h.work, "--size", "40"}, "ROWSxCOLS"},
		{[]string{"run", "fakei", "--interactive", "--cwd", h.work, "--size", "0x80"}, "ROWSxCOLS"},
		{[]string{"run", "fakei", "--cwd", h.work, "--prompt", "x", "--size", "40x80"}, "need --interactive"},
		{[]string{"run", "fakei", "--cwd", h.work, "--prompt", "x", "--attach"}, "need --interactive"},
		{[]string{"run", "fakei", "--interactive", "--remote", "--restart", "on-reboot", "--cwd", h.work}, "#55"},
		{[]string{"run", "fakei", "--interactive", "--follow", "--cwd", h.work}, "--attach"},
		{[]string{"run", "fakei", "--interactive", "--attach", "--cwd", h.work}, "terminal on stdin"},
	} {
		_, errOut, code := h.cmd(c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 2 with %q", c.args, code, errOut, c.want)
		}
	}
	if out, _, _ := h.cmd("ps", "--all", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("a refused run left a session: %s", out)
	}
}

// TestRunInteractiveNoPromptDefaultSize: without a prompt or a terminal the
// TUI starts at 24x80 with no prompt argument and no prompt.txt.
func TestRunInteractiveNoPromptDefaultSize(t *testing.T) {
	h := newInteractiveHarness(t)
	h.ok("run", "fakenp", "--interactive", "--cwd", h.work, "--name", "n1")
	logs := waitLog(t, h, "n1", "size=24x80")
	if !strings.Contains(logs, "argv=--tui|--auto\r") {
		t.Fatalf("argv: %q", logs)
	}
	if v := inspectI(t, h, "n1"); v.Plan.PTY == nil || v.Plan.PTY.Rows != 24 || v.Plan.PTY.Cols != 80 {
		t.Fatalf("plan: %+v", v.Plan)
	}
}

// trustDialogFixture is the trust dialog Claude Code 2.1.322 really draws.
func trustDialogFixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../profile/testdata/claude-2.1.322-trust-dialog.raw")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunInteractiveRemotePromptQuotingDialog: a prompt that quotes the
// trust dialog, echoed by the TUI, is not mistaken for the dialog, whether
// it quotes the question alone or the heading and the option together.
func TestRunInteractiveRemotePromptQuotingDialog(t *testing.T) {
	h := newInteractiveHarness(t)
	for i, prompt := range []string{
		"Claude asked me: is this a project you created or one you trust? What does that mean?",
		"It said Quick safety check: Is this a project you created or one you trust? and offered Yes, I trust this folder. Should I?",
	} {
		name := fmt.Sprintf("q%d", i)
		// FAKE_PTY_ARGV echoes the prompt (as the TUI would) before the URL.
		out, errOut, code := h.cmd("run", "fakei", "--interactive", "--remote", "--cwd", h.work, "--name", name,
			"--env", "FAKE_PTY_RUN=url", "--prompt", prompt)
		if code != 0 || !strings.HasPrefix(out, name+"  https://claude.ai/code/session_") {
			t.Fatalf("prompt %d: exit %d, out %q, err %q", i, code, out, errOut)
		}
		if logs := h.ok("logs", name); !strings.Contains(logs, prompt) {
			t.Fatalf("prompt %d not echoed: %q", i, logs)
		}
	}
}

// TestRunInteractiveRemoteShortPromptKeepsDialog: a short prompt made of
// the dialog's own words does not hide the real dialog.
func TestRunInteractiveRemoteShortPromptKeepsDialog(t *testing.T) {
	h := newInteractiveHarness(t)
	for i, prompt := range []string{"trust", "Quick safety check", "Yes, I trust this folder"} {
		_, errOut, code := h.cmd("run", "fakei", "--interactive", "--remote", "--cwd", h.work, "--name", fmt.Sprintf("s%d", i),
			"--prompt", prompt, "--env", "FAKE_PTY_RUN=cat "+trustDialogFixture(t))
		if code != 1 || !strings.Contains(errOut, "is not a trusted workspace") {
			t.Errorf("prompt %q hid the dialog: exit %d: %s", prompt, code, errOut)
		}
	}
}
