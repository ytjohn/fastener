package cli_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

// Every store here is synthetic: bookkeeping lines and a cwd field.

// randomID is a fresh conversation ID, so tests running at once never see
// each other's agents in the process check.
func randomID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// claudeTranscript writes a synthetic transcript of id for cwd in the
// Claude store conf, last written at mtime (zero: an hour ago).
func claudeTranscript(t *testing.T, conf, cwd, id string, mtime time.Time) string {
	t.Helper()
	physical, _ := filepath.EvalSymlinks(cwd)
	path := filepath.Join(claudeStoreDir(conf, cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session-owner"}` + "\n" + `{"type":"user","cwd":"` + physical + `"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if mtime.IsZero() {
		mtime = time.Now().Add(-time.Hour)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func newAdoptHarness(t *testing.T) (h *harness, conf string) {
	t.Helper()
	h = newHarness(t)
	h.writeProfile("fadopt", fakeResumeProfile("fadopt", `session_id = ["--sid", "{session_id}"]`, false))
	return h, t.TempDir()
}

type adoptInspect struct {
	Session struct {
		Cwd     string `json:"cwd"`
		Kind    string `json:"kind"`
		Adopted bool   `json:"adopted"`
		Restart string `json:"restart"`
	} `json:"session"`
}

// TestAdoptEndToEnd: adopt without --cwd takes the folder from the
// transcript, resumes the conversation through [resume] on a pty, records
// it as the session's exact conversation_id, marks the session adopted, and
// restore after a reboot resumes it again. While the lost session holds the
// conversation, a second adopt is refused.
func TestAdoptEndToEnd(t *testing.T) {
	h, conf := newAdoptHarness(t)
	id := randomID(t)
	claudeTranscript(t, conf, h.work, id, time.Time{})
	h.ok("adopt", "fadopt", id, "--name", "ad1", "--restart", "on-reboot", "--size", "30x90", "--env", "CLAUDE_CONFIG_DIR="+conf)

	physical, _ := filepath.EvalSymlinks(h.work)
	var ai adoptInspect
	if err := json.Unmarshal([]byte(h.ok("inspect", "ad1", "--json")), &ai); err != nil {
		t.Fatal(err)
	}
	if ai.Session.Cwd != physical || ai.Session.Kind != session.KindInteractive || !ai.Session.Adopted || ai.Session.Restart != "on-reboot" {
		t.Fatalf("session: %+v", ai.Session)
	}
	v := inspectR(t, h, "ad1")
	if v.State != session.StateRunning || v.Plan.SessionID != id || !argvHas(v.Plan.Argv, "--tui", "--resume", id, "--auto") ||
		argvHas(v.Plan.Argv, "--sid") || v.Plan.PTY == nil || v.Plan.PTY.Rows != 30 {
		t.Fatalf("plan: %+v", v)
	}
	poll(t, "conversation_id", func() bool {
		s := inspectR(t, h, "ad1").Status
		return s.ConversationID == id && s.ConversationSource == "session_id"
	})
	if out := h.ok("inspect", "ad1"); !strings.Contains(out, "adopted") {
		t.Fatalf("inspect does not show adopted:\n%s", out)
	}

	reboot(t, h, "ad1")
	_, e, code := h.cmd("adopt", "fadopt", id, "--name", "ad2", "--env", "CLAUDE_CONFIG_DIR="+conf)
	if code != 1 || !strings.Contains(e, "already Fastener session ad1's") || !strings.Contains(e, "--force") {
		t.Fatalf("adopt of a held conversation: exit %d: %s", code, e)
	}
	if out := h.ok("restore", "ad1"); out != "ad1  restored  generation 2" {
		t.Fatalf("restore printed %q", out)
	}
	v = inspectR(t, h, "ad1")
	if v.Plan.Generation != 2 || v.Plan.SessionID != id || !argvHas(v.Plan.Argv, "--resume", id) {
		t.Fatalf("generation 2: %+v", v)
	}
}

// TestAdoptCwdMismatch: a --cwd other than the transcript's folder is
// refused, and nothing is created; the transcript's own folder is accepted.
func TestAdoptCwdMismatch(t *testing.T) {
	h, conf := newAdoptHarness(t)
	id := randomID(t)
	claudeTranscript(t, conf, h.work, id, time.Time{})
	other := t.TempDir()
	_, e, code := h.cmd("adopt", "fadopt", id, "--cwd", other, "--env", "CLAUDE_CONFIG_DIR="+conf)
	physical, _ := filepath.EvalSymlinks(h.work)
	if code != 1 || !strings.Contains(e, "belongs to "+physical) || !strings.Contains(e, "not "+other) {
		t.Fatalf("mismatch: exit %d: %s", code, e)
	}
	if out := h.ok("ps", "--all", "--json"); out != "[]" {
		t.Fatalf("a session was created: %s", out)
	}
	h.ok("adopt", "fadopt", id, "--cwd", h.work, "--name", "m1", "--env", "CLAUDE_CONFIG_DIR="+conf)
}

// TestAdoptInUseGuard: a transcript written within the last few minutes, or
// a process with the ID in its arguments, makes adopt refuse; --force
// adopts anyway.
func TestAdoptInUseGuard(t *testing.T) {
	h, conf := newAdoptHarness(t)
	id := randomID(t)
	claudeTranscript(t, conf, h.work, id, time.Now().Add(-30*time.Second))
	_, e, code := h.cmd("adopt", "fadopt", id, "--env", "CLAUDE_CONFIG_DIR="+conf)
	if code != 1 || !strings.Contains(e, "looks in use") || !strings.Contains(e, "written") ||
		!strings.Contains(e, "close the other session, or pass --force") {
		t.Fatalf("recent transcript: exit %d: %s", code, e)
	}

	id2 := randomID(t)
	claudeTranscript(t, conf, h.work, id2, time.Time{})
	// "; :" keeps sh alive (not exec'ing sleep), with the ID in its argv.
	other := exec.Command("sh", "-c", "sleep 60; :", "--resume="+id2)
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	_, e, code = h.cmd("adopt", "fadopt", id2, "--env", "CLAUDE_CONFIG_DIR="+conf)
	if code != 1 || !strings.Contains(e, fmt.Sprintf("process %d", other.Process.Pid)) || !strings.Contains(e, "--force") {
		t.Fatalf("process holding it: exit %d: %s", code, e)
	}

	h.ok("adopt", "fadopt", id, "--force", "--name", "g1", "--env", "CLAUDE_CONFIG_DIR="+conf)
	h.ok("adopt", "fadopt", id2, "--force", "--name", "g2", "--env", "CLAUDE_CONFIG_DIR="+conf)
}

// TestAdoptNotFound: a conversation the store does not have exits 127.
func TestAdoptNotFound(t *testing.T) {
	h, conf := newAdoptHarness(t)
	id := randomID(t)
	_, e, code := h.cmd("adopt", "fadopt", id, "--env", "CLAUDE_CONFIG_DIR="+conf)
	if code != 127 || !strings.Contains(e, "has no conversation "+id) {
		t.Fatalf("exit %d: %s", code, e)
	}
	// Under another folder's key is still "found" (and then refused as a
	// mismatch); a different ID is not.
	claudeTranscript(t, conf, h.work, randomID(t), time.Time{})
	if _, _, code := h.cmd("adopt", "fadopt", id, "--cwd", h.work, "--env", "CLAUDE_CONFIG_DIR="+conf); code != 127 {
		t.Fatalf("exit %d", code)
	}
}

// TestAdoptUsage: arguments, the ID's shape and run-only flags are usage
// errors; -h prints adopt's own, grouped usage.
func TestAdoptUsage(t *testing.T) {
	h, _ := newAdoptHarness(t)
	id := randomID(t)
	for _, args := range [][]string{
		{"adopt", "fadopt"},
		{"adopt", "fadopt", "not-a-uuid"},
		{"adopt", "fadopt", "--" + id},
		{"adopt", "fadopt", id, "--prompt", "hi"},
		{"adopt", "fadopt", id, "--follow"},
		{"adopt", "fadopt", id, "--max-runtime", "1m"},
		{"adopt", "fadopt", id, "--restart", "always"},
		{"adopt", "fadopt", id, "--size", "big"},
		{"adopt", "fadopt", id, "--attach", "--json"},
	} {
		if _, e, code := h.cmd(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, code, e)
		}
	}
	_, e, code := h.cmd("adopt", "-h")
	if code != 0 || !strings.Contains(e, "usage: fastener adopt <backend> <conversation-id>") || !strings.Contains(e, "Folder:") ||
		strings.Contains(e, "-max-runtime") {
		t.Fatalf("-h: exit %d:\n%s", code, e)
	}
	if _, e, code := h.cmd("adopt", "nosuchbackend", id); code != 127 {
		t.Fatalf("unknown backend: exit %d: %s", code, e)
	}
}

// fakeCodexProfile stands in for codex: a finder over a synthetic Codex
// store and `resume <id>` as a subcommand.
func fakeCodexProfile(bin string) string {
	return fmt.Sprintf(`schema_version = 1
name    = "fcodex"
command = %q

[headless]
args   = []
prompt = "stdin"
parser = "codex-jsonl"

[permissions]
auto = ["--auto"]

[resume]
args       = ["resume", "{conversation_id}"]
subcommand = true

[interactive]
args   = ["--tui"]
prompt = "arg"
env    = ["FAKE_PTY=1", "FAKE_PTY_ARGV=1"]
finder = "codex-rollouts"
`, bin)
}

// TestAdoptCodexNeedsCwd: Codex's finder gives no folder, so --cwd is
// required; with it, the rollout is resumed with `resume <id>`.
func TestAdoptCodexNeedsCwd(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("fcodex", fakeCodexProfile(fakeBin))
	codex := t.TempDir()
	id := randomID(t)
	path := filepath.Join(codex, "sessions", "2026", "11", "01", "rollout-2026-11-01T10-00-00-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-11-01T15:00:00Z","cwd":"/somewhere","source":"cli"}}` + "\n"
	if err := os.WriteFile(path, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	_, e, code := h.cmd("adopt", "fcodex", id, "--env", "CODEX_HOME="+codex)
	if code != 2 || !strings.Contains(e, "--cwd is required") {
		t.Fatalf("no --cwd: exit %d: %s", code, e)
	}
	h.ok("adopt", "fcodex", id, "--cwd", h.work, "--name", "cx1", "--env", "CODEX_HOME="+codex)
	v := inspectR(t, h, "cx1")
	if len(v.Plan.Argv) < 3 || v.Plan.Argv[1] != "resume" || v.Plan.Argv[2] != id || v.Plan.SessionID != id {
		t.Fatalf("plan: %+v", v.Plan)
	}
	poll(t, "conversation_id", func() bool { return inspectR(t, h, "cx1").Status.ConversationID == id })
}
