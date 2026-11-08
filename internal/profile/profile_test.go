package profile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	os.WriteFile(filepath.Join(dir, "claude.toml"), []byte(`schema_version = 1
name = "claude"
command = "/opt/claude"
[headless]
args = []
prompt = "stdin"
parser = "none"
[permissions]
auto = []
`), 0o600)
	p, err := Load("claude", dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Command != "/opt/claude" || p.Source != filepath.Join(dir, "claude.toml") || len(p.Check.Diagnose) != 0 {
		t.Fatalf("user profile not used wholesale: %+v", p)
	}
}

func TestValidationErrors(t *testing.T) {
	base := "schema_version = 1\nname = \"x\"\ncommand = \"x\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n"
	cases := map[string]string{
		"unknown key":    base + "bogus = 1\n",
		"unknown parser": strings.Replace(base, `parser = "none"`, `parser = "nope"`, 1),
		"bad prompt":     strings.Replace(base, `prompt = "stdin"`, `prompt = "file"`, 1),
		"bad level":      base + "weird = []\n",
		"bad regex":      base + "[check]\ndiagnose = [{ match = '(', hint = \"h\" }]\n",
		"no command":     strings.Replace(base, `command = "x"`, `command = ""`, 1),
		"interactive":    base + "[interactive]\nargs = []\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load("nonexistent", t.TempDir()); !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("unknown backend: %v", err)
	}
}

func TestRenderHeadless(t *testing.T) {
	p, _ := Load("claude", t.TempDir())
	r, err := p.RenderHeadless(Request{Permissions: "auto", Model: "haiku", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "auto", "--model", "haiku"}
	if !reflect.DeepEqual(r.Args, want) || r.PromptMode != "stdin" || r.Parser != "claude-stream-json" {
		t.Fatalf("render = %+v", r)
	}
	// Placeholder substitution stays inside one argument.
	r, _ = p.RenderHeadless(Request{Permissions: "auto", Model: "a b; rm -rf /"})
	if r.Args[len(r.Args)-1] != "a b; rm -rf /" {
		t.Fatalf("model split or altered: %q", r.Args)
	}
}

func TestRenderNoSilentFallback(t *testing.T) {
	pi, _ := Load("pi", t.TempDir())
	if _, err := pi.RenderHeadless(Request{Permissions: "auto"}); err == nil || !strings.Contains(err.Error(), "skip") {
		t.Fatalf("pi auto: %v (want error naming defined levels)", err)
	}
	r, err := pi.RenderHeadless(Request{Permissions: "skip", Prompt: "-rf hello"})
	if err != nil {
		t.Fatal(err)
	}
	n := len(r.Args)
	if r.Args[n-2] != "--" || r.Args[n-1] != "-rf hello" {
		t.Fatalf("arg prompt not guarded by --: %q", r.Args)
	}
	if _, err := pi.RenderHeadless(Request{Permissions: "skip", Model: "--evil"}); err == nil {
		t.Fatal("model starting with '-' accepted")
	}
	noModel, _ := Parse([]byte("schema_version = 1\nname = \"x\"\ncommand = \"x\"\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"none\"\n[permissions]\nauto = []\n"))
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
