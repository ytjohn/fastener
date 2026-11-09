package profile

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeRemoteRender(t *testing.T) {
	p, err := Load("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"auto": {"remote-control", "--name", "bridge-1", "--permission-mode", "auto"},
		// remote-control has no --dangerously-skip-permissions.
		"skip": {"remote-control", "--name", "bridge-1", "--permission-mode", "bypassPermissions"},
	}
	for lvl, want := range cases {
		r, err := p.RenderRemote(Request{Permissions: lvl, Name: "bridge-1"})
		if err != nil {
			t.Fatalf("%s: %v", lvl, err)
		}
		if !reflect.DeepEqual(r.Args, want) {
			t.Errorf("%s: args = %q, want %q", lvl, r.Args, want)
		}
		if p.Remote.UntrustedHint != "fastener authorize claude-folder-trust {dir}" {
			t.Errorf("untrusted_hint = %q", p.Remote.UntrustedHint)
		}
		if r.PromptMode != PromptNone || r.Parser != "claude-remote" || !r.DedupeFrames {
			t.Errorf("%s: %+v", lvl, r)
		}
	}
	// The headless mapping is unchanged.
	h, err := p.RenderHeadless(Request{Permissions: "skip"})
	if err != nil || !strings.Contains(strings.Join(h.Args, " "), "--dangerously-skip-permissions") {
		t.Errorf("headless skip: %q %v", h.Args, err)
	}
	if _, err := p.RenderRemote(Request{Permissions: "auto", Name: "b", Model: "opus"}); err == nil {
		t.Error("--model with --remote rendered")
	}
}

func TestRemoteNameStaysOneArgument(t *testing.T) {
	p, err := Parse([]byte(baseTOML + "[remote]\nargs = [\"serve\", \"--label={name}\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.RenderRemote(Request{Permissions: "auto", Name: "a b --evil"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"serve", "--label=a b --evil"}; !reflect.DeepEqual(r.Args, want) {
		t.Errorf("args = %q, want %q", r.Args, want)
	}
	if r.Parser != DefaultRemoteParser || r.DedupeFrames {
		t.Errorf("defaults: %+v", r)
	}
}

func TestNoRemoteSection(t *testing.T) {
	for _, name := range []string{"codex", "pi"} {
		p, err := Load(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.RenderRemote(Request{Permissions: "skip", Name: "x"})
		if !errors.Is(err, ErrNoRemote) || !strings.Contains(err.Error(), "backend "+name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRemoteValidation(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"empty args", "[remote]\nargs = []\n", "remote.args must not be empty"},
		{"name twice", "[remote]\nargs = [\"{name}\", \"--n={name}\"]\n", "at most once"},
		{"unknown parser", "[remote]\nargs = [\"x\"]\nparser = \"nope\"\n", "remote.parser"},
		{"unknown key", "[remote]\nargs = [\"x\"]\nprompt = \"stdin\"\n", "unknown keys"},
		{"hint without pattern", "[remote]\nargs = [\"x\"]\nuntrusted_hint = \"trust {dir}\"\n", "needs remote.untrusted_pattern"},
		{"bad pattern", "[remote]\nargs = [\"x\"]\nuntrusted_pattern = \"(\"\n", "untrusted_pattern"},
		{"unknown level", "[remote]\nargs = [\"x\"]\n[remote.permissions]\nweird = []\n", "unknown level"},
		{"level not in permissions", "[remote]\nargs = [\"x\"]\n[remote.permissions]\nskip = []\n", "not defined in [permissions]"},
		{"value in remote args", "[remote]\nargs = [\"{value}\"]\n", "only allowed in options"},
		{"name in remote permissions", "[remote]\nargs = [\"x\"]\n[remote.permissions]\nauto = [\"{name}\"]\n", "only allowed in remote.args"},
		{"name in headless args", "[remote]\nargs = [\"x\"]\n", "only allowed in remote.args"},
	}
	for _, c := range cases {
		src := baseTOML + c.src
		if c.name == "name in headless args" {
			src = strings.Replace(src, "args = []", "args = [\"{name}\"]", 1)
		}
		_, err := Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := Parse([]byte(baseTOML + "[remote]\nargs = [\"x\"]\n")); err != nil {
		t.Errorf("no {name} is allowed: %v", err)
	}
}
