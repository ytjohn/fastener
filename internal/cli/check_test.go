package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type checkJSON struct {
	Backend string `json:"backend"`
	Passed  bool   `json:"passed"`
	Session string `json:"session"`
	Skipped bool   `json:"skipped"`
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

func stepDetail(r checkJSON, name string) string {
	for _, s := range r.Steps {
		if s.Name == name {
			return s.Detail
		}
	}
	return ""
}

func stepNames(r checkJSON) []string {
	var n []string
	for _, s := range r.Steps {
		n = append(n, s.Name)
	}
	return n
}

// writeBuiltinStandIns shadows claude/codex/pi with fake-backed profiles (pi
// defines only "skip"), so --all tests do not depend on installed CLIs.
func (h *harness) writeBuiltinStandIns() {
	for _, n := range []string{"claude", "codex"} {
		h.writeProfile(n, strings.Replace(fmt.Sprintf(fakeProfile, fakeBin), `name    = "fake"`, `name    = "`+n+`"`, 1))
	}
	h.writeProfile("pi", fmt.Sprintf("schema_version = 1\nname = \"pi\"\ncommand = %q\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nskip = []\n", fakeBin))
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
	// The fake reports no model unless FAKE_MODEL is set.
	if st, _ := stepStatus(r[0], "model"); st != "skip" || stepDetail(r[0], "model") != "backend does not report its model" {
		t.Errorf("model step = %q %q", st, stepDetail(r[0], "model"))
	}
	if n := stepNames(r[0]); strings.Join(n, ",") != "profile,binary,launch,exit,parse,model,reply,cleanup" {
		t.Errorf("step order: %v", n)
	}
	if _, _, c := h.cmd("inspect", r[0].Session); c != 127 {
		t.Error("check session not removed")
	}
	if n := stepNames(r[0]); n[len(n)-1] != "cleanup" {
		t.Errorf("cleanup is not the last step: %v", n)
	}
	if m, _ := filepath.Glob(filepath.Join(h.work, "fastener-check-*")); len(m) != 0 {
		t.Errorf("temporary cwd left behind: %v", m)
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
	r, _ := h.check("fake", "--env", "FAKE_IS_ERROR=1", "--env", "FAKE_REPLY=model_not_supported")
	if st, _ := stepStatus(r[0], "parse"); st != "fail" {
		t.Fatalf("parse %q", st)
	}
	if d := stepDetail(r[0], "parse"); !strings.HasPrefix(d, "agent reported an error: model_not_supported") {
		t.Errorf("parse detail lacks the error text: %q", d)
	}
	r, _ = h.check("fake", "--env", "FAKE_IS_ERROR=1", "--env", "FAKE_REPLY="+strings.Repeat("x", 500))
	if d := stepDetail(r[0], "parse"); !strings.Contains(d, strings.Repeat("x", 200)+"…") || strings.Contains(d, strings.Repeat("x", 201)) {
		t.Errorf("long error text not truncated to 200: %d bytes", len(d))
	}
	r, _ = h.check("fake", "--env", "FAKE_IS_ERROR=1", "--env", "FAKE_REPLY=")
	if d := stepDetail(r[0], "parse"); !strings.Contains(d, "agent reported an error") || strings.Contains(d, "agent reported an error:") {
		t.Errorf("empty error text: %q", d)
	}
}

func TestCheckDiagnosesKnownFailure(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--env", "FAKE_STDERR=FAKE-NOT-LOGGED-IN", "--env", "FAKE_EXIT=1")
	st, hint := stepStatus(r[0], "exit")
	if code != 1 || st != "fail" || hint != "auth: log in to fake" {
		t.Fatalf("code %d, exit=%q hint=%q", code, st, hint)
	}
	if d := stepDetail(r[0], "exit"); !strings.Contains(d, "FAKE-NOT-LOGGED-IN") {
		t.Errorf("matched hint lost the stderr tail: %q", d)
	}
	for _, n := range stepNames(r[0]) {
		if n == "parse" || n == "reply" {
			t.Errorf("step %s ran after an exit failure: %v", n, stepNames(r[0]))
		}
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
	h.writeBuiltinStandIns()
	r, code := h.check("--all", "--static")
	if code != 0 {
		t.Fatalf("exit %d: %+v", code, r)
	}
	for _, rep := range r {
		if rep.Backend == "fake" && rep.Passed {
			return
		}
	}
	t.Fatalf("fake missing or failing in --all: %+v", r)
}

func TestCheckAllSkipsUndefinedPermissionLevel(t *testing.T) {
	h := newHarness(t)
	h.writeBuiltinStandIns()
	r, code := h.check("--all", "--static")
	if code != 0 {
		t.Fatalf("--all exit %d: %+v", code, r)
	}
	var pi *checkJSON
	for i := range r {
		if r[i].Backend == "pi" {
			pi = &r[i]
		}
	}
	if pi == nil || !pi.Skipped || pi.Passed || len(pi.Steps) != 1 {
		t.Fatalf("pi report: %+v", pi)
	}
	if st, _ := stepStatus(*pi, "profile"); st != "skip" || !strings.Contains(stepDetail(*pi, "profile"), "pass --permissions") {
		t.Errorf("profile step: %+v", pi.Steps)
	}
	// A single explicit check of the same backend is a real failure.
	r, code = h.check("pi", "--static")
	if code != 1 || r[0].Skipped {
		t.Fatalf("single pi check: code %d, %+v", code, r)
	}
	// And --permissions skip checks it properly under --all.
	r, code = h.check("--all", "--static", "--permissions", "skip")
	if code != 0 {
		t.Fatalf("--all --permissions skip: %d %+v", code, r)
	}
}

func TestCheckEchoedPromptDoesNotPass(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	h := newHarness(t)
	h.writeProfile("echo", fmt.Sprintf("schema_version = 1\nname = \"echo\"\ncommand = %q\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n", cat))
	r, code := h.check("echo")
	if st, _ := stepStatus(r[0], "reply"); code != 1 || st != "fail" {
		t.Fatalf("code %d, reply %q: %+v", code, st, r[0].Steps)
	}
}

func TestCheckNoneParser(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("fakenone", strings.NewReplacer(`parser = "claude-stream-json"`, `parser = "none"`, `name    = "fake"`, `name    = "fakenone"`).Replace(fmt.Sprintf(fakeProfile, fakeBin)))
	r, code := h.check("fakenone")
	if code != 0 {
		t.Fatalf("code %d: %+v", code, r[0].Steps)
	}
	if st, _ := stepStatus(r[0], "parse"); st != "skip" {
		t.Errorf("parse = %q", st)
	}
	if st, _ := stepStatus(r[0], "reply"); st != "pass" {
		t.Errorf("reply = %q", st)
	}
}

func TestCheckTimeoutHintBeatsPatternsOnTokenCounts(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("tok", `schema_version = 1
name = "tok"
command = "sh"
[headless]
args = ["-c", "echo '{\"input_tokens\":14012}'; exec sleep 30"]
prompt = "stdin"
parser = "none"
[permissions]
auto = []
[check]
diagnose = [ { match = '401', hint = "auth: wrongly matched" } ]
`)
	r, _ := h.check("tok", "--max-runtime", "1s", "--grace", "1s")
	if _, hint := stepStatus(r[0], "exit"); !strings.Contains(hint, "interactive input") {
		t.Fatalf("hint %q", hint)
	}
}

func TestCheckModelReplyNeverTriggersHint(t *testing.T) {
	h := newHarness(t)
	r, _ := h.check("fake", "--env", "FAKE_REPLY=FAKE-NOT-LOGGED-IN")
	if st, hint := stepStatus(r[0], "reply"); st != "fail" || hint != "" {
		t.Fatalf("reply %q hint %q", st, hint)
	}
}

func TestCheckKeepKeepsTempCwd(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--keep")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	m, _ := filepath.Glob(filepath.Join(h.work, "fastener-check-*"))
	if len(m) != 1 || !strings.Contains(stepDetail(r[0], "cleanup"), m[0]) {
		t.Fatalf("cwd %v, cleanup detail %q", m, stepDetail(r[0], "cleanup"))
	}
	_ = os.RemoveAll(m[0])
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

func TestCheckAllNeverPassesVacuously(t *testing.T) {
	h := newHarness(t)
	// Only skip-only backends exist besides fake; remove fake's auto level by
	// shadowing everything with skip-only profiles.
	for _, n := range []string{"fake", "claude", "codex", "pi"} {
		h.writeProfile(n, fmt.Sprintf("schema_version = 1\nname = %q\ncommand = %q\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nskip = []\n", n, fakeBin))
	}
	out, errOut, code := h.cmd("check", "--all", "--static")
	if code != 1 || !strings.Contains(errOut, "no backend was checked") {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "SKIP pi") {
		t.Errorf("skipped report not labelled SKIP:\n%s", out)
	}
}

func TestPermissionsValueValidated(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"check", "--all", "--static", "--permissions", "bogus"},
		{"check", "fake", "--static", "--permissions", "bogus"},
		{"run", "fake", "--cwd", h.work, "--prompt", "x", "--permissions", "bogus"},
	} {
		if _, _, code := h.cmd(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestCheckStaticValidatesLauncher(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--static", "--launcher", "bogus")
	if code != 1 || r[0].Passed {
		t.Fatalf("code %d: %+v", code, r)
	}
}

// skipDefaultProfile defines only "skip" and makes it the profile's default.
func skipDefaultProfile(name string) string {
	return fmt.Sprintf("schema_version = 1\nname = %q\ncommand = %q\ndefault_permissions = \"skip\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"claude-stream-json\"\n[permissions]\nskip = []\n", name, fakeBin)
}

func TestProfileDefaultPermissions(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("skipdef", skipDefaultProfile("skipdef"))
	h.ok("run", "skipdef", "--name", "d1", "--cwd", h.work, "--prompt", "x")
	var v struct {
		Session struct {
			Permissions string `json:"permissions"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(h.ok("inspect", "d1", "--json")), &v); err != nil || v.Session.Permissions != "skip" {
		t.Fatalf("session permissions = %q (%v)", v.Session.Permissions, err)
	}
	// An explicit --permissions auto still errors.
	if _, errOut, code := h.cmd("run", "skipdef", "--cwd", h.work, "--prompt", "x", "--permissions", "auto"); code != 1 || !strings.Contains(errOut, `permission level "auto"`) {
		t.Fatalf("explicit auto: code %d: %s", code, errOut)
	}
	r, code := h.check("skipdef", "--static")
	if code != 0 || !strings.Contains(stepDetail(r[0], "profile"), "permissions=skip") {
		t.Fatalf("check: code %d %+v", code, r)
	}
	// The config's default_permissions beats the profile's.
	if err := os.WriteFile(filepath.Join(h.config, "config.toml"), []byte("default_permissions = \"auto\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := h.cmd("run", "skipdef", "--cwd", h.work, "--prompt", "x"); code != 1 || !strings.Contains(errOut, `permission level "auto"`) {
		t.Fatalf("config auto: code %d: %s", code, errOut)
	}
	if r, code := h.check("skipdef", "--static"); code != 1 || r[0].Skipped {
		t.Fatalf("check with config auto: code %d %+v", code, r)
	}
	// And the flag beats the config.
	if r, code := h.check("skipdef", "--static", "--permissions", "skip"); code != 0 {
		t.Fatalf("flag skip: code %d %+v", code, r)
	}
}

func TestCheckAllChecksBackendWithDefaultLevel(t *testing.T) {
	h := newHarness(t)
	h.writeBuiltinStandIns()
	h.writeProfile("pi", skipDefaultProfile("pi"))
	r, code := h.check("--all", "--static")
	if code != 0 {
		t.Fatalf("exit %d: %+v", code, r)
	}
	for _, rep := range r {
		if rep.Backend == "pi" {
			if rep.Skipped || !rep.Passed {
				t.Fatalf("pi skipped or failed: %+v", rep)
			}
			return
		}
	}
	t.Fatal("pi missing from --all")
}

func TestCheckModelStep(t *testing.T) {
	h := newHarness(t)
	r, code := h.check("fake", "--model", "claude-haiku", "--env", "FAKE_MODEL=claude-haiku-4-5-20251001")
	if code != 0 || !r[0].Passed {
		t.Fatalf("code %d: %+v", code, r)
	}
	if st, _ := stepStatus(r[0], "model"); st != "pass" || stepDetail(r[0], "model") != "requested claude-haiku, served claude-haiku-4-5-20251001" {
		t.Errorf("model step %q %q", st, stepDetail(r[0], "model"))
	}

	r, code = h.check("fake", "--model", "flash-next", "--env", "FAKE_MODEL=glm-5.3-flash-next")
	st, hint := stepStatus(r[0], "model")
	if code != 1 || r[0].Passed || st != "fail" || !strings.Contains(stepDetail(r[0], "model"), "but the backend served glm-5.3-flash-next") {
		t.Fatalf("mismatch: code %d, model %q: %+v", code, st, r[0].Steps)
	}
	if !strings.Contains(hint, "--model glm-5.3-flash-next") || !strings.Contains(hint, "--expect-model") {
		t.Errorf("hint %q", hint)
	}
	for _, n := range stepNames(r[0]) {
		if n == "reply" {
			t.Errorf("reply ran after a model failure: %v", stepNames(r[0]))
		}
	}

	r, code = h.check("fake", "--model", "flash-next", "--expect-model", "glm", "--env", "FAKE_MODEL=glm-5.3-flash-next")
	if code != 0 || !r[0].Passed {
		t.Fatalf("--expect-model: code %d: %+v", code, r[0].Steps)
	}
	if d := stepDetail(r[0], "model"); d != "expected glm, served glm-5.3-flash-next" {
		t.Errorf("--expect-model detail %q", d)
	}

	r, code = h.check("fake", "--model", "claude-haiku")
	if st, _ := stepStatus(r[0], "model"); code != 0 || st != "skip" || !strings.Contains(stepDetail(r[0], "model"), "could not be verified") {
		t.Errorf("unreported model: code %d, %q %q", code, st, stepDetail(r[0], "model"))
	}

	r, code = h.check("fake", "--env", "FAKE_MODEL=claude-opus-5-5")
	if st, _ := stepStatus(r[0], "model"); code != 0 || st != "pass" || stepDetail(r[0], "model") != "served claude-opus-5-5" {
		t.Errorf("no expectation: code %d, %q %q", code, st, stepDetail(r[0], "model"))
	}
}

// piShProfile is a pi-json backend that prints one canned run: a session event
// and an assistant message_end built from msg (a JSON object body).
func piShProfile(name, msg string) string {
	script := `cat >/dev/null; printf '%s\n' '{"type":"session","id":"sess-1"}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"pong"}],"stopReason":"stop",` + msg + `}}'`
	return fmt.Sprintf("schema_version = 1\nname = %q\ncommand = \"sh\"\n[headless]\nargs = [\"-c\", %q]\nprompt = \"stdin\"\nparser = \"pi-json\"\n[permissions]\nauto = []\n", name, script)
}

func TestCheckPiResponseModelMismatch(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("picomp", piShProfile("picomp", `"api":"openai-completions","provider":"p","model":"qwen3-x","responseModel":"glm-5"`))
	r, code := h.check("picomp", "--expect-model", "qwen3")
	if st, _ := stepStatus(r[0], "model"); code != 1 || st != "fail" || !strings.Contains(stepDetail(r[0], "model"), "served p/glm-5") {
		t.Fatalf("code %d: %+v", code, r[0].Steps)
	}
}

func TestCheckPiClientReportedModel(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("piresp", piShProfile("piresp", `"api":"openai-responses","provider":"p","model":"gpt-5.6"`))
	r, code := h.check("piresp", "--expect-model", "gpt-5")
	if code != 0 {
		t.Fatalf("code %d: %+v", code, r[0].Steps)
	}
	if d := stepDetail(r[0], "model"); !strings.Contains(d, "served p/gpt-5.6 (client-reported") {
		t.Errorf("model detail %q", d)
	}
	var v struct {
		Status struct {
			ModelSource string `json:"model_source"`
		} `json:"status"`
	}
	r, _ = h.check("piresp", "--keep")
	if err := json.Unmarshal([]byte(h.ok("inspect", r[0].Session, "--json")), &v); err != nil || v.Status.ModelSource != "client" {
		t.Errorf("model_source = %q (%v)", v.Status.ModelSource, err)
	}
}

func TestCheckExpectationUsageErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"check", "fake", "--expect-model", "github-copilot/"},
		{"check", "fake", "--expect-model", "/"},
		{"check", "fake", "--expect-model", ""},
		{"check", "fake", "--model", "github-copilot/"},
	} {
		if _, errOut, code := h.cmd(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errOut)
		}
	}
}

func TestCheckStaticWarnsAboutExpectModel(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.cmd("check", "fake", "--static", "--expect-model", "x")
	if code != 0 || !strings.Contains(errOut, "--expect-model has no effect") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}
