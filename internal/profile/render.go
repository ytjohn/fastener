package profile

import (
	"fmt"
	"strings"
)

type Request struct {
	Permissions string
	Model       string
	Prompt      string
}

// Rendered is a headless invocation without the command itself (argv[1:]).
type Rendered struct {
	Args       []string
	PromptMode string
	Parser     string
}

// RenderHeadless builds the argument list. Nothing passes through a shell;
// placeholders are substituted within single arguments. Anything requested
// that the profile does not define is an error, never a fallback.
func (p *Profile) RenderHeadless(r Request) (Rendered, error) {
	perm, ok := p.Permissions[r.Permissions]
	if !ok {
		return Rendered{}, fmt.Errorf("backend %s does not define permission level %q (defined: %s)",
			p.Name, r.Permissions, strings.Join(p.Levels(), ", "))
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
		args = append(args, "--", r.Prompt)
	}
	return Rendered{Args: args, PromptMode: p.Headless.Prompt, Parser: p.Headless.Parser}, nil
}
