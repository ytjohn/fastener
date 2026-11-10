package cli_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

// fakeResumeProfile is fakeagent's interactive profile with a finder over a
// synthetic Claude store and [resume]; sid is the session_id line (or "").
func fakeResumeProfile(name, sid string, remote bool) string {
	extra := ""
	if remote {
		extra = fakeInteractiveRemote
	}
	return fmt.Sprintf(`schema_version = 1
name    = %q
command = %q

[headless]
args   = []
prompt = "stdin"
parser = "claude-stream-json"

[permissions]
auto = ["--auto"]

[resume]
args = ["--resume", "{conversation_id}"]

[interactive]
args   = ["--tui"]
prompt = "arg"
env    = ["FAKE_PTY=1", "FAKE_PTY_ARGV=1"]
finder = "claude-transcripts"
%s
%s`, name, fakeBin, sid, extra)
}

// claudeStoreDir is the synthetic Claude project directory for cwd.
func claudeStoreDir(conf, cwd string) string {
	physical, _ := filepath.EvalSymlinks(cwd)
	key := []byte(physical)
	for i, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			key[i] = '-'
		}
	}
	return filepath.Join(conf, "projects", string(key))
}

type irInspect struct {
	State   string `json:"state"`
	Orphans int    `json:"orphans"`
	Plan    struct {
		Generation int      `json:"generation"`
		Argv       []string `json:"argv"`
		SessionID  string   `json:"session_id"`
		PTY        *struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		} `json:"pty"`
	} `json:"plan"`
	Status struct {
		Generation            int    `json:"generation"`
		PGID                  int    `json:"pgid"`
		ConversationID        string `json:"conversation_id"`
		ConversationCandidate string `json:"conversation_candidate"`
		ConversationSource    string `json:"conversation_source"`
		RestoreReason         string `json:"restore_reason"`
		RemoteURL             string `json:"remote_url"`
		Monitor               *struct {
			PID int `json:"pid"`
		} `json:"monitor"`
	} `json:"status"`
}

func inspectR(t *testing.T, h *harness, name string) irInspect {
	t.Helper()
	var v irInspect
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// reboot kills a running session's monitor and agent as a reboot would,
// and waits for it to read lost with nothing left.
func reboot(t *testing.T, h *harness, name string) {
	t.Helper()
	v := inspectR(t, h, name)
	if v.Status.Monitor == nil || v.Status.PGID == 0 {
		t.Fatalf("%s not running: %+v", name, v)
	}
	_ = syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	_ = syscall.Kill(-v.Status.PGID, syscall.SIGKILL)
	poll(t, name+" lost", func() bool {
		v := inspectR(t, h, name)
		return v.State == session.StateLost && v.Orphans == 0
	})
}

func argvHas(argv []string, seq ...string) bool {
	return strings.Contains(" "+strings.Join(argv, " ")+" ", " "+strings.Join(seq, " ")+" ")
}

// TestRestoreInteractiveResumes: an interactive --restart on-reboot session
// whose conversation is known (the assigned ID, written) is relaunched as
// the next generation with [resume] and that ID, at the same terminal size,
// and can be attached to.
func TestRestoreInteractiveResumes(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("fres", fakeResumeProfile("fres", `session_id = ["--sid", "{session_id}"]`, false))
	conf := t.TempDir()
	dir := claudeStoreDir(conf, h.work)
	h.ok("run", "fres", "--interactive", "--restart", "on-reboot", "--cwd", h.work, "--name", "r1", "--size", "30x90",
		"--env", "CLAUDE_CONFIG_DIR="+conf, "--env", "FAKE_PTY_RUN=touchsid "+dir)
	sid := inspectR(t, h, "r1").Plan.SessionID
	poll(t, "conversation_id", func() bool { return inspectR(t, h, "r1").Status.ConversationID == sid })
	reboot(t, h, "r1")

	if out := h.ok("restore", "r1"); out != "r1  restored  generation 2" {
		t.Fatalf("restore printed %q", out)
	}
	v := inspectR(t, h, "r1")
	if v.State != session.StateRunning || v.Plan.Generation != 2 || v.Plan.SessionID != sid ||
		!argvHas(v.Plan.Argv, "--tui", "--resume", sid, "--auto") || argvHas(v.Plan.Argv, "--sid") ||
		v.Plan.PTY == nil || v.Plan.PTY.Rows != 30 || v.Plan.PTY.Cols != 90 {
		t.Fatalf("generation 2: %+v", v)
	}
	poll(t, "generation 2's conversation_id", func() bool {
		s := inspectR(t, h, "r1").Status
		return s.ConversationID == sid && s.ConversationSource == "session_id"
	})
	a := startAttach(t, h, "r1", "--quiet")
	a.waitOutput("winch=30x100")
	a.typeIn("again\r")
	a.waitOutput("echo:again")
	a.typeIn("\x1d")
	a.wait()
}

// TestRestoreInteractiveNoConversation: an interactive session nothing was
// ever sent to has no conversation; restore fails clearly rather than start
// a fresh one.
func TestRestoreInteractiveNoConversation(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("fres", fakeResumeProfile("fres", `session_id = ["--sid", "{session_id}"]`, false))
	h.ok("run", "fres", "--interactive", "--restart", "on-reboot", "--cwd", h.work, "--name", "r2",
		"--env", "CLAUDE_CONFIG_DIR="+t.TempDir())
	reboot(t, h, "r2")
	out, _, code := h.cmd("restore", "r2", "--json")
	var res []struct {
		Result, Reason, Message string
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 1 || len(res) != 1 ||
		res[0].Result != "failed" || res[0].Reason != "no_conversation" || !strings.Contains(res[0].Message, "never starts a fresh one") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if v := inspectR(t, h, "r2"); v.State != session.StateRestoreFailed || v.Status.RestoreReason != "no_conversation" || v.Plan.Generation != 1 {
		t.Fatalf("after the failed restore: %+v", v)
	}
}

// TestRestoreInteractiveCandidate: a heuristic finder's candidate is resumed
// only with --accept-candidate, which needs the session named.
func TestRestoreInteractiveCandidate(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("fcand", fakeResumeProfile("fcand", "", false))
	conf := t.TempDir()
	const cand = "11111111-2222-4333-8444-555555555555"
	h.ok("run", "fcand", "--interactive", "--restart", "on-reboot", "--cwd", h.work, "--name", "r3",
		"--env", "CLAUDE_CONFIG_DIR="+conf, "--env", "FAKE_PTY_RUN=touch "+filepath.Join(claudeStoreDir(conf, h.work), cand+".jsonl"))
	poll(t, "candidate", func() bool { return inspectR(t, h, "r3").Status.ConversationCandidate == cand })
	if v := inspectR(t, h, "r3"); v.Status.ConversationID != "" {
		t.Fatalf("a heuristic answer became conversation_id: %+v", v.Status)
	}
	reboot(t, h, "r3")

	_, errOut, code := h.cmd("restore", "r3")
	if code != 1 || !strings.Contains(errOut+h.ok("inspect", "r3"), "unconfirmed_candidate") {
		t.Fatalf("restore without --accept-candidate: exit %d %s", code, errOut)
	}
	if _, errOut, code := h.cmd("restore", "--accept-candidate"); code != 2 || !strings.Contains(errOut, "named") {
		t.Fatalf("--accept-candidate without names: exit %d %s", code, errOut)
	}
	if out := h.ok("restore", "--accept-candidate", "r3"); out != "r3  restored  generation 2" {
		t.Fatalf("restore --accept-candidate printed %q", out)
	}
	if v := inspectR(t, h, "r3"); !argvHas(v.Plan.Argv, "--resume", cand) || v.Plan.SessionID != cand {
		t.Fatalf("generation 2: %+v", v.Plan)
	}
}

// TestRestoreInteractiveRemote: a remote interactive session's restore waits
// for the new generation's URL and prints it, as run --interactive --remote
// does.
func TestRestoreInteractiveRemote(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("frem", fakeResumeProfile("frem", `session_id = ["--sid", "{session_id}"]`, true))
	conf := t.TempDir()
	dir := claudeStoreDir(conf, h.work)
	const url = "https://claude.ai/code/session_01FAKEPTYSESSION0001"
	if out := h.ok("run", "frem", "--interactive", "--remote", "--restart", "on-reboot", "--cwd", h.work, "--name", "r4",
		"--env", "CLAUDE_CONFIG_DIR="+conf, "--env", "FAKE_PTY_RUN=touchsid "+dir+"; url"); out != "r4  "+url {
		t.Fatalf("run printed %q", out)
	}
	sid := inspectR(t, h, "r4").Plan.SessionID
	poll(t, "conversation_id", func() bool { return inspectR(t, h, "r4").Status.ConversationID == sid })
	reboot(t, h, "r4")
	if out := h.ok("restore", "r4"); out != "r4  restored  generation 2  "+url {
		t.Fatalf("restore printed %q", out)
	}
	v := inspectR(t, h, "r4")
	if !argvHas(v.Plan.Argv, "--tui", "--resume", sid, "--remote-control", "r4", "--auto") || v.Status.RemoteURL != url {
		t.Fatalf("generation 2: %+v", v)
	}
}

// TestRestoreInteractiveRefusesWhileOldAlive: restore never relaunches
// while a process of the old generation lives: here the agent, whose monitor
// was killed. (On Linux the agent dies with its monitor, PR_SET_PDEATHSIG,
// so this runs on macOS; the scan itself is the bridges' one.)
func TestRestoreInteractiveRefusesWhileOldAlive(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("fres", fakeResumeProfile("fres", `session_id = ["--sid", "{session_id}"]`, false))
	conf := t.TempDir()
	h.ok("run", "fres", "--interactive", "--restart", "on-reboot", "--cwd", h.work, "--name", "r5",
		"--env", "CLAUDE_CONFIG_DIR="+conf, "--env", "FAKE_PTY_RUN=touchsid "+claudeStoreDir(conf, h.work))
	v := inspectR(t, h, "r5")
	agent := v.Status.PGID
	t.Cleanup(func() { _ = syscall.Kill(-agent, syscall.SIGKILL) })
	_ = syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	poll(t, "lost", func() bool { return inspectR(t, h, "r5").State == session.StateLost })
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(agent) {
		t.Skip("the agent died with its monitor (PR_SET_PDEATHSIG)")
	}
	out := h.ok("restore", "r5")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "stop it first") {
		t.Fatalf("restore with the old agent alive: %q", out)
	}
	if v := inspectR(t, h, "r5"); v.Plan.Generation != 1 {
		t.Fatalf("relaunched beside the old agent: %+v", v.Plan)
	}
}
