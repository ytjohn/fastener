package profile

import (
	"errors"
	"fmt"
	"strings"
)

type Request struct {
	Permissions string
	Model       string
	Prompt      string
	Name        string // the session name, for remote.args' {name}
}

// ErrNoRemote is wrapped by RenderRemote when the profile has no [remote]
// section.
var ErrNoRemote = errors.New("no [remote] section")

// PromptNone is the prompt mode of a remote bridge: it reads no prompt and
// its stdin is /dev/null.
const PromptNone = "none"

type noRemoteError struct{ backend string }

func (e noRemoteError) Error() string {
	return fmt.Sprintf("backend %s has no [remote] section, so it does not support --remote", e.backend)
}
func (e noRemoteError) Is(target error) bool { return target == ErrNoRemote }

// Rendered is a headless invocation without the command itself (argv[1:]).
type Rendered struct {
	Args       []string
	PromptMode string
	Parser     string
	// DedupeFrames: the monitor drops redrawn lines from output.log.
	DedupeFrames bool
}

// RenderHeadless builds the argument list. Nothing passes through a shell;
// placeholders are substituted within single arguments. Anything requested
// that the profile does not define is an error, never a fallback.
func (p *Profile) RenderHeadless(r Request) (Rendered, error) {
	perm, err := p.permArgs(r.Permissions, nil)
	if err != nil {
		return Rendered{}, err
	}
	args := append([]string{}, p.Headless.Args...)
	args = append(args, perm...)
	if r.Model != "" {
		if strings.HasPrefix(r.Model, "-") {
			return Rendered{}, fmt.Errorf("model %q must not start with '-'", r.Model)
		}
		tmpl, ok := p.Options["model"]
		if !ok {
			return Rendered{}, fmt.Errorf("backend %s does not support --model", p.Name)
		}
		for _, a := range tmpl {
			args = append(args, strings.ReplaceAll(a, "{value}", r.Model))
		}
	}
	if p.Headless.Prompt == "arg" {
		if strings.TrimSpace(r.Prompt) == "" {
			return Rendered{}, fmt.Errorf("backend %s takes the prompt as an argument; prompt is empty", p.Name)
		}
		args = append(args, "--", r.Prompt)
	}
	return Rendered{Args: args, PromptMode: p.Headless.Prompt, Parser: p.Headless.Parser}, nil
}

func (p *Profile) permArgs(level string, override map[string][]string) ([]string, error) {
	perm, ok := p.Permissions[level]
	if !ok {
		return nil, noLevelError{fmt.Sprintf("backend %s does not define permission level %q (defined: %s)",
			p.Name, level, strings.Join(p.Levels(), ", "))}
	}
	if o, ok := override[level]; ok {
		perm = o
	}
	return perm, nil
}

// RenderRemote builds a remote bridge's argument list: [remote].args with
// {name} replaced by the session name, then the permission level's
// arguments ([remote.permissions] if it lists the level, else
// [permissions]). A bridge takes no prompt and no options.
func (p *Profile) RenderRemote(r Request) (Rendered, error) {
	if p.Remote == nil {
		return Rendered{}, noRemoteError{p.Name}
	}
	perm, err := p.permArgs(r.Permissions, p.Remote.Permissions)
	if err != nil {
		return Rendered{}, err
	}
	if r.Model != "" {
		return Rendered{}, fmt.Errorf("backend %s does not support --model with --remote", p.Name)
	}
	var args []string
	for _, a := range p.Remote.Args {
		args = append(args, strings.ReplaceAll(a, namePH, r.Name))
	}
	args = append(args, perm...)
	return Rendered{Args: args, PromptMode: PromptNone, Parser: p.Remote.RemoteParser(),
		DedupeFrames: p.Remote.DedupeFrames}, nil
}
