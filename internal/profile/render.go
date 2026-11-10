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
	// Env is extra KEY=VALUE pairs for the agent ([interactive].env).
	Env []string
	// URLPattern is matched against terminal output to find remote_url
	// ([interactive.remote].url_pattern).
	URLPattern string
}

// ErrNoInteractive is wrapped by RenderInteractive when the profile has no
// [interactive] section (or, with remote, no [interactive.remote]).
var ErrNoInteractive = errors.New("no [interactive] section")

type noInteractiveError struct{ msg string }

func (e noInteractiveError) Error() string        { return e.msg }
func (e noInteractiveError) Is(target error) bool { return target == ErrNoInteractive }

// RenderInteractive builds an interactive run's argument list:
// [interactive].args, then (remote) [interactive.remote].args with {name}
// replaced, then the permission level's arguments ([interactive.permissions]
// if it lists the level, else [permissions]), then the model option, then,
// when a prompt is given and the profile takes one as an argument, "--" and
// the prompt. The prompt is optional.
func (p *Profile) RenderInteractive(r Request, remote bool) (Rendered, error) {
	in := p.Interactive
	if in == nil {
		return Rendered{}, noInteractiveError{fmt.Sprintf("backend %s has no [interactive] section, so it does not support --interactive", p.Name)}
	}
	if remote && in.Remote == nil {
		return Rendered{}, noInteractiveError{fmt.Sprintf("backend %s has no [interactive.remote] section, so it does not support --interactive --remote", p.Name)}
	}
	perm, err := p.permArgs(r.Permissions, in.Permissions)
	if err != nil {
		return Rendered{}, err
	}
	args := append([]string{}, in.Args...)
	out := Rendered{PromptMode: PromptNone, Parser: in.InteractiveParser(), Env: append([]string(nil), in.Env...)}
	if remote {
		for _, a := range in.Remote.Args {
			args = append(args, strings.ReplaceAll(a, namePH, r.Name))
		}
		out.URLPattern = in.Remote.URLPattern
	}
	args = append(args, perm...)
	if args, err = p.appendModel(args, r.Model); err != nil {
		return Rendered{}, err
	}
	if strings.TrimSpace(r.Prompt) != "" {
		if in.Prompt != InteractivePromptArg {
			return Rendered{}, fmt.Errorf("backend %s takes no initial prompt in interactive mode; type it after attaching", p.Name)
		}
		args = append(args, "--", r.Prompt)
		out.PromptMode = InteractivePromptArg
	}
	out.Args = args
	return out, nil
}

// appendModel appends the [options] model template for model, if one is set.
func (p *Profile) appendModel(args []string, model string) ([]string, error) {
	if model == "" {
		return args, nil
	}
	if strings.HasPrefix(model, "-") {
		return nil, fmt.Errorf("model %q must not start with '-'", model)
	}
	tmpl, ok := p.Options["model"]
	if !ok {
		return nil, fmt.Errorf("backend %s does not support --model", p.Name)
	}
	for _, a := range tmpl {
		args = append(args, strings.ReplaceAll(a, "{value}", model))
	}
	return args, nil
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
	if args, err = p.appendModel(args, r.Model); err != nil {
		return Rendered{}, err
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
