package profile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ytjohn/fastener/internal/parser"
)

func TestBuiltinsLoadAndValidate(t *testing.T) {
	for _, name := range []string{"claude", "codex", "pi"} {
		p, err := Load(name, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Source != "builtin" || len(p.SHA256) != 64 {
			t.Errorf("%s: source=%q sha=%q", name, p.Source, p.SHA256)
		}
	}
	names, err := Names(t.TempDir())
	if err != nil || !reflect.DeepEqual(names, []string{"claude", "codex", "pi"}) {
		t.Fatalf("Names = %v, %v", names, err)
	}
}

func TestUserProfileReplacesBuiltin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude.toml"), []byte(`schema_version = 1
name = "claude"
command = "/opt/claude"
[headless]
args = []
prompt = "stdin"
parser = "none"
[permissions]
auto = []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("claude", dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Command != "/opt/claude" || p.Source != filepath.Join(dir, "claude.toml") || len(p.Check.Diagnose) != 0 {
		t.Fatalf("user profile not used wholesale: %+v", p)
	}
}

const baseTOML = "schema_version = 1\nname = \"x\"\ncommand = \"x\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n"

func TestValidationErrors(t *testing.T) {
	base := baseTOML
	cases := []struct{ name, src, want string }{
		{"unknown key", "bogus = 1\n" + base, "unknown keys"},
		{"unknown parser", strings.Replace(base, `parser = "none"`, `parser = "nope"`, 1), "parser"},
		{"bad prompt", strings.Replace(base, `prompt = "stdin"`, `prompt = "file"`, 1), "headless.prompt"},
		{"bad level", base + "weird = []\n", "unknown level"},
		{"unknown option", base + "[options]\nfoo = [\"{value}\"]\n", "unknown option"},
		{"bad regex", base + "[check]\ndiagnose = [{ match = '(', hint = \"h\" }]\n", "match"},
		{"no hint", base + "[check]\ndiagnose = [{ match = 'a' }]\n", "hint is required"},
		{"no command", strings.Replace(base, `command = "x"`, `command = ""`, 1), "required"},
		{"interactive no prompt", base + "[interactive]\nargs = []\n", "interactive.prompt"},
		{"interactive bad prompt", base + "[interactive]\nprompt = \"stdin\"\n", "interactive.prompt"},
		{"interactive unknown key", base + "[interactive]\nprompt = \"arg\"\nbogus = 1\n", "unknown keys"},
		{"interactive bad parser", base + "[interactive]\nprompt = \"arg\"\nparser = \"nope\"\n", "interactive.parser"},
		{"interactive bad env", base + "[interactive]\nprompt = \"arg\"\nenv = [\"NOEQUALS\"]\n", "interactive.env"},
		{"interactive bad level", base + "[interactive]\nprompt = \"arg\"\n[interactive.permissions]\nskip = []\n", "not defined"},
		{"interactive name in args", base + "[interactive]\nprompt = \"arg\"\nargs = [\"{name}\"]\n", "{name}"},
		{"interactive value in args", base + "[interactive]\nprompt = \"arg\"\nargs = [\"{value}\"]\n", "placeholders"},
		{"interactive remote no args", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nurl_pattern = 'x'\n", "interactive.remote.args"},
		{"interactive remote no pattern", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nargs = [\"--rc\"]\n", "url_pattern is required"},
		{"interactive remote bad pattern", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nargs = [\"--rc\"]\nurl_pattern = '('\n", "url_pattern"},
		{"interactive remote name twice", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nargs = [\"{name}\", \"{name}\"]\nurl_pattern = 'x'\n", "at most once"},
		{"interactive remote hint alone", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nargs = [\"--rc\"]\nurl_pattern = 'x'\nuntrusted_hint = \"h\"\n", "needs"},
		{"interactive remote bad untrusted", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nargs = [\"--rc\"]\nurl_pattern = 'x'\nuntrusted_pattern = '('\n", "untrusted_pattern"},
		{"interactive unknown finder", base + "[interactive]\nprompt = \"arg\"\nfinder = \"nope\"\n", "interactive.finder"},
		{"interactive session_id without placeholder", base + "[interactive]\nprompt = \"arg\"\nsession_id = [\"--session-id\"]\n", "exactly once"},
		{"interactive session_id empty", base + "[interactive]\nprompt = \"arg\"\nsession_id = []\n", "exactly once"},
		{"interactive session_id twice", base + "[interactive]\nprompt = \"arg\"\nsession_id = [\"{session_id}\", \"{session_id}\"]\n", "exactly once"},
		{"session_id placeholder elsewhere", base + "[interactive]\nprompt = \"arg\"\nargs = [\"{session_id}\"]\n", "only allowed in interactive.session_id"},
		{"session_id placeholder in headless", strings.Replace(base, "args = []", `args = ["{session_id}"]`, 1), "only allowed in interactive.session_id"},
		{"name in session_id", base + "[interactive]\nprompt = \"arg\"\nsession_id = [\"{session_id}{name}\"]\n", "only the {session_id}"},
		{"interactive remote unknown key", base + "[interactive]\nprompt = \"arg\"\n[interactive.remote]\nargs = [\"--rc\"]\nurl_pattern = 'x'\nwhat = 1\n", "unknown keys"},
		{"empty permissions", strings.Replace(base, "auto = []\n", "", 1), "at least one level"},
		{"schema 2", strings.Replace(base, "schema_version = 1", "schema_version = 2", 1), "schema_version"},
		{"model without value", base + "[options]\nmodel = [\"--model\"]\n", "exactly once"},
		{"empty model template", base + "[options]\nmodel = []\n", "exactly once"},
		{"value twice", base + "[options]\nmodel = [\"{value}\", \"{value}\"]\n", "exactly once"},
		{"value in headless args", strings.Replace(base, "args = []", `args = ["{value}"]`, 1), "placeholders"},
		{"value in permissions", strings.Replace(base, "auto = []", `auto = ["{value}"]`, 1), "placeholders"},
		{"resume without id", base + "[resume]\nargs = [\"--resume\"]\n", "conversation_id"},
		{"resume empty", base + "[resume]\nargs = []\n", "conversation_id"},
		{"conversation_id in headless", strings.Replace(base, "args = []", `args = ["{conversation_id}"]`, 1), "placeholders"},
		{"relative command", strings.Replace(base, `command = "x"`, `command = "bin/agent"`, 1), "command"},
		{"command with space", strings.Replace(base, `command = "x"`, `command = "/opt/my agent"`, 1), "whitespace"},
		{"command dot", strings.Replace(base, `command = "x"`, `command = "."`, 1), "command"},
		{"command dotdot", strings.Replace(base, `command = "x"`, `command = ".."`, 1), "command"},
		{"command leading dash", strings.Replace(base, `command = "x"`, `command = "-x"`, 1), "command"},
		{"default level undefined", "default_permissions = \"skip\"\n" + base, "default_permissions"},
		{"default level unknown", "default_permissions = \"yolo\"\n" + base, "default_permissions"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.src))
		if err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q lacks %q", c.name, err, c.want)
		}
	}
	if _, err := Parse([]byte(strings.Replace(base, `command = "x"`, `command = "/opt/agent"`, 1))); err != nil {
		t.Errorf("absolute command rejected: %v", err)
	}
	if _, err := Load("nonexistent", t.TempDir()); !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("unknown backend: %v", err)
	}
	for _, n := range []string{"../x", "Claude"} {
		if _, err := Load(n, t.TempDir()); !errors.Is(err, ErrUnknownBackend) {
			t.Errorf("name %q: %v", n, err)
		}
	}
}

func TestLoadUserFileProblems(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude.toml"), []byte(strings.Replace(baseTOML, `name = "x"`, `name = "other"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("claude", dir); err == nil || !strings.Contains(err.Error(), "name field") {
		t.Errorf("name mismatch: %v", err)
	}
	// A directory where the file should be: error, not the built-in.
	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, "claude.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("claude", dir2); err == nil {
		t.Error("directory claude.toml silently fell back")
	}
	// User-only profile is listed.
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, "mine.toml"), []byte(strings.Replace(baseTOML, `name = "x"`, `name = "mine"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := Names(dir3)
	if err != nil || !reflect.DeepEqual(names, []string{"claude", "codex", "mine", "pi"}) {
		t.Fatalf("Names = %v, %v", names, err)
	}
}

// argModeProfile is an inline profile that takes its prompt as an argument.
// No built-in does any more (Pi moved to stdin), but the mode is supported.
func argModeProfile(t *testing.T) *Profile {
	t.Helper()
	p, err := Parse([]byte("schema_version = 1\nname = \"argy\"\ncommand = \"argy\"\n[headless]\nargs = [\"-p\"]\nprompt = \"arg\"\nparser = \"none\"\n[options]\nmodel = [\"--model\", \"{value}\"]\n[permissions]\nskip = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArgPromptGuard(t *testing.T) {
	pi := argModeProfile(t)
	r, err := pi.RenderHeadless(Request{Permissions: "skip", Prompt: "a\n-- b"})
	if err != nil {
		t.Fatal(err)
	}
	n := len(r.Args)
	if r.Args[n-2] != "--" || r.Args[n-1] != "a\n-- b" {
		t.Fatalf("prompt not a single guarded arg: %q", r.Args)
	}
	for _, p := range []string{"", "  \n\t"} {
		if _, err := pi.RenderHeadless(Request{Permissions: "skip", Prompt: p}); err == nil {
			t.Errorf("empty prompt %q accepted", p)
		}
	}
}

func TestRenderHeadless(t *testing.T) {
	p, err := Load("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.RenderHeadless(Request{Permissions: "auto", Model: "haiku", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "auto", "--model", "haiku"}
	if !reflect.DeepEqual(r.Args, want) || r.PromptMode != "stdin" || r.Parser != "claude-stream-json" {
		t.Fatalf("render = %+v", r)
	}
	// Placeholder substitution stays inside one argument.
	r, err = p.RenderHeadless(Request{Permissions: "auto", Model: "a b; rm -rf /"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Args[len(r.Args)-1] != "a b; rm -rf /" {
		t.Fatalf("model split or altered: %q", r.Args)
	}
}

func TestRenderNoSilentFallback(t *testing.T) {
	pi, err := Load("pi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pi.RenderHeadless(Request{Permissions: "auto"}); err == nil || !strings.Contains(err.Error(), "skip") {
		t.Fatalf("pi auto: %v (want error naming defined levels)", err)
	}
	argy := argModeProfile(t)
	r, err := argy.RenderHeadless(Request{Permissions: "skip", Prompt: "-rf hello"})
	if err != nil {
		t.Fatal(err)
	}
	n := len(r.Args)
	if r.Args[n-2] != "--" || r.Args[n-1] != "-rf hello" {
		t.Fatalf("arg prompt not guarded by --: %q", r.Args)
	}
	// Pi's prompt goes on stdin, so a leading "@" (a file include to pi) never reaches its argv.
	r, err = pi.RenderHeadless(Request{Permissions: "skip", Prompt: "@team please review"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Args {
		if strings.Contains(a, "@team") {
			t.Fatalf("pi prompt leaked into argv: %q", r.Args)
		}
	}
	if _, err := pi.RenderHeadless(Request{Permissions: "skip", Model: "--evil", Prompt: "p"}); err == nil {
		t.Fatal("model starting with '-' accepted")
	}
	noModel, err := Parse([]byte("schema_version = 1\nname = \"x\"\ncommand = \"x\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noModel.RenderHeadless(Request{Permissions: "auto", Model: "m"}); err == nil {
		t.Fatal("--model accepted by a profile without options.model")
	}
}

func TestEmptyUserDirUsesBuiltinsOnly(t *testing.T) {
	p, err := Load("claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "builtin" {
		t.Fatalf("source = %q, want builtin", p.Source)
	}
	names, err := Names("")
	if err != nil || !reflect.DeepEqual(names, []string{"claude", "codex", "pi"}) {
		t.Fatalf("Names(\"\") = %v, %v", names, err)
	}
}

func TestBuiltinDiagnosePatterns(t *testing.T) {
	benign := []string{
		`"cached_input_tokens":4012`, `"input_tokens":14012`, `took 1.404s`,
		`/tmp/fastener-check-1401404`, `pong`,
		"the model then explained at length why the config file does not exist",
		"a report about the unauthorized access of customer data was discussed",
	}
	positive := map[string][]string{
		"codex": {"HTTP 401 Unauthorized", "status: 401", "Not logged in"},
		"pi":    {"HTTP 401 from provider", "status: 404", "status code 404"},
		"claude": {"Invalid API key", `{"type":"not_found_error"}`, "model claude-x does not exist",
			"Error: Workspace not trusted. Please run `claude` in /x first to review and accept the workspace trust dialog."},
	}
	for _, name := range []string{"claude", "codex", "pi"} {
		p, err := Load(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Check.Diagnose {
			re := regexp.MustCompile(d.Match)
			for _, b := range benign {
				if re.MatchString(b) {
					t.Errorf("%s: %q matches benign %q", name, d.Match, b)
				}
			}
		}
		for _, s := range positive[name] {
			hit := false
			for _, d := range p.Check.Diagnose {
				hit = hit || regexp.MustCompile(d.Match).MatchString(s)
			}
			if !hit {
				t.Errorf("%s: no pattern matches %q", name, s)
			}
		}
	}
}

func TestNoLevelErrorMessageOnce(t *testing.T) {
	p, err := Load("pi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.RenderHeadless(Request{Permissions: "auto", Prompt: "x"})
	if !errors.Is(err, ErrNoPermissionLevel) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), ErrNoPermissionLevel.Error()) {
		t.Errorf("sentinel text leaks into the message: %v", err)
	}
}

func TestDefaultPermissionsValid(t *testing.T) {
	p, err := Parse([]byte("default_permissions = \"auto\"\n" + baseTOML))
	if err != nil || p.DefaultPermissions != "auto" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

func TestResolvePermissionsPrecedence(t *testing.T) {
	withDefault := &Profile{DefaultPermissions: "skip"}
	without := &Profile{}
	cases := []struct {
		name            string
		p               *Profile
		flag, cfg, want string
	}{
		{"flag beats all", withDefault, "auto", "skip", "auto"},
		{"config beats profile", withDefault, "", "auto", "auto"},
		{"profile default", withDefault, "", "", "skip"},
		{"auto last", without, "", "", "auto"},
		{"config without profile default", without, "", "skip", "skip"},
		{"flag without profile default", without, "skip", "", "skip"},
	}
	for _, c := range cases {
		if got := c.p.ResolvePermissions(c.flag, c.cfg); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPiRendersJSONWithDefaultSkip(t *testing.T) {
	pi, err := Load("pi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	level := pi.ResolvePermissions("", "")
	if level != "skip" {
		t.Fatalf("pi default level = %q, want skip", level)
	}
	r, err := pi.RenderHeadless(Request{Permissions: level, Model: "github-copilot/claude-haiku-4.5", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--mode", "json", "--model", "github-copilot/claude-haiku-4.5"}
	if !reflect.DeepEqual(r.Args, want) || r.Parser != "pi-json" || r.PromptMode != "stdin" {
		t.Fatalf("render = %+v", r)
	}
	// An explicit auto still errors: no silent fallback.
	if _, err := pi.RenderHeadless(Request{Permissions: pi.ResolvePermissions("auto", ""), Prompt: "hi"}); !errors.Is(err, ErrNoPermissionLevel) {
		t.Fatalf("explicit auto: %v", err)
	}
}

func TestPiModelNotFoundHint(t *testing.T) {
	pi, err := Load("pi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	line := `Warning: Model "claude-nonexistent-9" not found for provider "github-copilot". Using custom model id.`
	for _, d := range pi.Check.Diagnose {
		if regexp.MustCompile(d.Match).MatchString(line) {
			if !strings.Contains(d.Hint, "not offered") {
				t.Fatalf("first matching hint is %q", d.Hint)
			}
			return
		}
	}
	t.Fatal("no pattern matches pi's model-not-found warning")
}

func TestCodexUsesCodexJSONL(t *testing.T) {
	p, err := Load("codex", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p.Headless.Parser != "codex-jsonl" {
		t.Fatalf("parser = %q, want codex-jsonl", p.Headless.Parser)
	}
	msg := "The 'no-such-model-9' model is not supported when using Codex with a ChatGPT account."
	for _, d := range p.Check.Diagnose {
		if regexp.MustCompile(d.Match).MatchString(msg) {
			if d.Hint != "model not offered for this Codex account" {
				t.Fatalf("first matching hint = %q", d.Hint)
			}
			return
		}
	}
	t.Fatal("no diagnose entry matches the unsupported-model message")
}

func TestBuiltinInteractive(t *testing.T) {
	claude, err := Load("claude", "")
	if err != nil {
		t.Fatal(err)
	}
	const sid = "11111111-2222-4333-8444-555555555555"
	r, err := claude.RenderInteractive(Request{Permissions: "skip", Name: "s1", Prompt: "-hi", Model: "opus", SessionID: sid}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--session-id", sid, "--remote-control", "s1", "--dangerously-skip-permissions", "--model", "opus", "--", "-hi"}
	if strings.Join(r.Args, "|") != strings.Join(want, "|") || r.URLPattern == "" || r.Parser != "none" || r.PromptMode != "arg" ||
		r.SessionID != sid || r.Finder != "claude-transcripts" {
		t.Fatalf("claude interactive remote: %+v", r)
	}
	if !regexp.MustCompile(r.URLPattern).MatchString("https://claude.ai/code/session_017sf5rxuW4AeHi7B5SRz7ij") {
		t.Fatal("claude url_pattern misses a session URL")
	}
	if _, err := claude.RenderInteractive(Request{Permissions: "auto"}, false); err == nil {
		t.Fatal("claude rendered without the session ID it assigns")
	}
	if r, err := claude.RenderInteractive(Request{Permissions: "auto", SessionID: sid}, false); err != nil || strings.Join(r.Args, "|") != "--session-id|"+sid+"|--permission-mode|auto" || r.PromptMode != "none" {
		t.Fatalf("claude interactive: %+v %v", r, err)
	}
	pi, err := Load("pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := pi.RenderInteractive(Request{Permissions: "skip", SessionID: sid}, false); err != nil || strings.Join(r.Args, "|") != "--session-id|"+sid || r.Finder != "pi-sessions" {
		t.Fatalf("pi interactive: %+v %v", r, err)
	}
	if _, err := pi.RenderInteractive(Request{Permissions: "skip", Prompt: "hi", SessionID: sid}, false); !errors.Is(err, ErrNoInteractive) {
		t.Fatalf("pi with a prompt: %v", err)
	}
	if _, err := pi.RenderInteractive(Request{Permissions: "skip"}, true); !errors.Is(err, ErrNoInteractive) {
		t.Fatalf("pi --remote: %v", err)
	}
	codex, err := Load("codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := codex.RenderInteractive(Request{Permissions: "auto", Prompt: "go"}, false); err != nil || strings.Join(r.Args, "|") != "--approve-for-me|--|go" || r.Finder != "codex-rollouts" || r.SessionID != "" {
		t.Fatalf("codex interactive: %+v %v", r, err)
	}
}

// TestClaudeTrustDialogPattern: the interactive untrusted_pattern matches
// the dialog Claude Code 2.1.322 really draws, and not a prompt that only
// quotes its question.
func TestClaudeTrustDialogPattern(t *testing.T) {
	claude, err := Load("claude", "")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(claude.Interactive.Remote.UntrustedPattern)
	raw, err := os.ReadFile("testdata/claude-2.1.322-trust-dialog.raw")
	if err != nil {
		t.Fatal(err)
	}
	if !re.Match(parser.StripForMatch(raw)) {
		t.Fatal("the real trust dialog does not match")
	}
	for _, echo := range []string{
		"Claude asked me: is this a project you created or one you trust? What does that mean?",
		"Quick safety check: is this a project you created or one you trust?",
		"why does it say workspace not trusted",
		"Yes, I trust this folder",
	} {
		if re.MatchString(echo) {
			t.Errorf("a prompt echo matched: %q", echo)
		}
	}
}
