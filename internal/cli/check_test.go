package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

type checkJSON struct {
	Backend string `json:"backend"`
	Passed  bool   `json:"passed"`
	Session string `json:"session"`
	Steps   []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
		Hint   string `json:"hint"`
	} `json:"steps"`
}

func (h *harness) check(args ...string) ([]checkJSON, int) {
	h.t.Helper()
	out, errOut, code := h.cmd(append([]string{"check", "--json"}, args...)...)
	var r []checkJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		h.t.Fatalf("check output not JSON (exit %d): %s\n%s", code, out, errOut)
	}
	return r, code
}

func stepStatus(r checkJSON, name string) (string, string) {
	for _, s := range r.Steps {
		if s.Name == name {
			return s.Status, s.Hint
		}
	}
	return "", ""
}

func TestCheckPasses(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake")
	if code != 0 || !r[0].Passed {
		t.Fatalf("code %d: %+v", code, r)
	}
	for _, step := range []string{"profile", "binary", "launch", "exit", "parse", "reply", "cleanup"} {
		if st, _ := stepStatus(r[0], step); st != "pass" {
			t.Errorf("step %s = %q", step, st)
		}
	}
	if _, _, c := h.cmd("inspect", r[0].Session); c != 127 {
		t.Error("check session not removed")
	}
}

func TestCheckWrongReply(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--env", "FAKE_REPLY=nope")
	if st, _ := stepStatus(r[0], "reply"); code != 1 || st != "fail" {
		t.Fatalf("code %d, reply %q", code, st)
	}
}

func TestCheckAgentError(t *testing.T) {
	h := newHarness(t)
	r, _ := h.check("fake", "--env", "FAKE_IS_ERROR=1")
	if st, _ := stepStatus(r[0], "parse"); st != "fail" {
		t.Fatalf("parse %q", st)
	}
}

func TestCheckDiagnosesKnownFailure(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--env", "FAKE_STDERR=FAKE-NOT-LOGGED-IN", "--env", "FAKE_EXIT=1")
	st, hint := stepStatus(r[0], "exit")
	if code != 1 || st != "fail" || hint != "auth: log in to fake" {
		t.Fatalf("code %d, exit=%q hint=%q", code, st, hint)
	}
}

func TestCheckTimeoutHint(t *testing.T) {
	h := newHarness(t)
	r, _ := h.check("fake", "--env", "FAKE_SLEEP=30s", "--max-runtime", "1s", "--grace", "1s")
	_, hint := stepStatus(r[0], "exit")
	if !strings.Contains(hint, "interactive input") {
		t.Fatalf("hint %q", hint)
	}
}

func TestCheckMissingBinaryAndStatic(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("ghost", "schema_version = 1\nname = \"ghost\"\ncommand = \"ghost-agent-nowhere\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n")
	r, code := h.check("ghost")
	if st, _ := stepStatus(r[0], "binary"); code != 1 || st != "fail" {
		t.Fatalf("binary step %q code %d", st, code)
	}
	r, code = h.check("fake", "--static")
	if code != 0 || len(r[0].Steps) != 2 {
		t.Fatalf("static: code %d steps %+v", code, r[0].Steps)
	}
}

func TestCheckUnknownBackend(t *testing.T) {
	h := newHarness(t)
	if _, _, code := h.cmd("check", "nope"); code != 127 {
		t.Fatalf("code %d", code)
	}
}

func TestCheckAllStaticIncludesFake(t *testing.T) {
	h := newHarness(t)
	r, _ := h.check("--all", "--static") // built-ins may fail if their CLIs are absent
	for _, rep := range r {
		if rep.Backend == "fake" && rep.Passed {
			return
		}
	}
	t.Fatalf("fake missing or failing in --all: %+v", r)
}

func TestCheckRejectsBadFlags(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"check", "fake", "--max-runtime", "-1s"},
		{"check", "fake", "--grace", "-1s"},
		{"check", "fake", "--env", "BADKEY"},
		{"check"},
		{"check", "--all", "fake"},
	} {
		if _, _, code := h.cmd(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestCheckKeepLeavesSession(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--keep")
	if code != 0 || !r[0].Passed {
		t.Fatalf("code %d: %+v", code, r)
	}
	if _, _, c := h.cmd("inspect", r[0].Session); c != 0 {
		t.Error("kept session not inspectable")
	}
}
