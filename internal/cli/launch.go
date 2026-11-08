package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/launcher"
	"github.com/ytjohn/fastener/internal/profile"
	"github.com/ytjohn/fastener/internal/runenv"
	"github.com/ytjohn/fastener/internal/session"
	"github.com/ytjohn/fastener/internal/version"
)

type launchOptions struct {
	Backend, Name, Cwd string
	Prompt, PromptFile string
	Permissions, Model string
	EnvFile, Launcher  string
	Env                []string
	Labels             map[string]string
	MaxRuntime, Grace  time.Duration
}

// stepError tags a pre-flight error with the `check` step it belongs to.
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

func inStep(step string, err error) error { return &stepError{step: step, err: err} }

// prepared is everything resolved before a session directory exists.
type prepared struct {
	cfg      config.Config
	prof     *profile.Profile
	perms    string
	rendered profile.Rendered
	argv0    string
	envBase  []string
	envFile  string
	fileEnv  []string
	launcher launcher.Launcher
}

func (p *prepared) mergedEnv(extra []string) []string {
	return runenv.Merge(p.envBase, p.fileEnv, extra)
}

// prepare runs every check that can fail without side effects.
func prepare(o launchOptions, prompt string) (*prepared, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, inStep("profile", err)
	}
	prof, err := profile.Load(o.Backend, config.BackendsDir())
	if err != nil {
		return nil, inStep("profile", err)
	}
	perms := o.Permissions
	if perms == "" {
		perms = cfg.DefaultPermissions
	}
	if perms == "" {
		perms = "auto"
	}
	r, err := prof.RenderHeadless(profile.Request{Permissions: perms, Model: o.Model, Prompt: prompt})
	if err != nil {
		return nil, inStep("profile", err)
	}
	for _, kv := range o.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return nil, inStep("binary", usagef("--env %q: want KEY=VALUE", kv))
		}
	}
	p := &prepared{cfg: cfg, prof: prof, perms: perms, rendered: r}
	p.envBase = runenv.Base(cfg.EnvPassthrough, os.Environ())
	if o.EnvFile != "" {
		abs, err := filepath.Abs(o.EnvFile)
		if err != nil {
			return nil, inStep("binary", err)
		}
		if p.fileEnv, err = runenv.ReadFile(abs); err != nil {
			return nil, inStep("binary", fmt.Errorf("--env-file: %w", err))
		}
		p.envFile = abs
	}
	p.argv0, err = runenv.LookPath(prof.Command, runenv.Get(p.mergedEnv(o.Env), "PATH"))
	if err != nil {
		return nil, inStep("binary", err)
	}
	pref := o.Launcher
	if pref == "" {
		pref = cfg.Launcher
	}
	if p.launcher, err = launcher.Select(pref); err != nil {
		return nil, inStep("launch", err)
	}
	return p, nil
}

// launch creates a session and starts its monitor. On a handshake failure it
// returns the view (if one exists) together with the error.
func launch(o launchOptions) (*session.View, error) {
	prompt, source := o.Prompt, "inline"
	if o.PromptFile != "" {
		if prompt != "" {
			return nil, usagef("--prompt and --prompt-file are mutually exclusive")
		}
		abs, err := filepath.Abs(o.PromptFile)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		prompt, source = string(b), abs
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, usagef("a prompt is required (--prompt or --prompt-file)")
	}
	if o.Cwd == "" {
		return nil, usagef("--cwd is required")
	}
	cwd, err := filepath.Abs(o.Cwd)
	if err != nil {
		return nil, err
	}
	if o.Name != "" && !session.ValidName(o.Name) {
		return nil, usagef("invalid --name %q", o.Name)
	}
	p, err := prepare(o, prompt)
	if err != nil {
		return nil, err
	}
	name := o.Name
	if name == "" {
		name = session.GenerateName(o.Backend)
	}
	d, err := session.Create(name)
	if err != nil {
		return nil, inStep("launch", err)
	}
	now := time.Now().UTC()
	sess := session.Session{
		SchemaVersion: session.SchemaVersion, Name: name, CreatedAt: now, Kind: "headless",
		Backend: o.Backend, Cwd: cwd, PromptSource: source, Permissions: p.perms, Model: o.Model,
		MaxRuntime: session.Duration(o.MaxRuntime), Grace: session.Duration(o.Grace),
		EnvBase: p.envBase, Env: o.Env, EnvFile: p.envFile, Labels: o.Labels, Restart: "no",
	}
	plan := session.Plan{
		SchemaVersion: session.SchemaVersion, Generation: 1,
		Argv: append([]string{p.argv0}, p.rendered.Args...), PromptMode: p.rendered.PromptMode,
		Parser: p.rendered.Parser, ProfileSource: p.prof.Source, ProfileSHA256: p.prof.SHA256,
		Marker: session.NewMarker(name, 1), FastenerVersion: version.Version, PlannedAt: now,
	}
	writes := []struct {
		path string
		v    any
	}{
		{d.SessionPath(), sess},
		{d.PlanPath(1), plan},
		{d.StatusPath(), session.Status{SchemaVersion: session.SchemaVersion, State: session.StateStarting, Generation: 1, UpdatedAt: now}},
	}
	if err := session.WriteFileAtomic(d.PromptPath(), []byte(prompt), 0o600); err != nil {
		return nil, inStep("launch", err)
	}
	for _, w := range writes {
		if err := session.WriteJSON(w.path, w.v); err != nil {
			return nil, inStep("launch", err)
		}
	}
	rec, err := p.launcher.Start(d, 1)
	if err != nil {
		_, _ = session.Finalize(d, func(s *session.Status) {
			t := time.Now().UTC()
			s.State, s.EndedBy, s.EndedAt, s.Error = session.StateExited, session.EndedByLaunchFailed, &t, "monitor: "+err.Error()
		})
		return nil, inStep("launch", fmt.Errorf("start monitor: %w", err))
	}
	if err := session.WriteJSON(d.LauncherPath(1), rec); err != nil {
		return nil, inStep("launch", err)
	}
	return handshake(name, p.cfg.Handshake())
}

func handshake(name string, timeout time.Duration) (*session.View, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := session.Load(name, timeout)
		if err != nil {
			return nil, inStep("launch", err)
		}
		switch v.State {
		case session.StateRunning:
			return v, nil
		case session.StateExited:
			if v.Status.EndedBy == session.EndedByLaunchFailed {
				return v, inStep("launch", fmt.Errorf("launch failed: %s", v.Status.Error))
			}
			return v, nil // a fast agent may already have finished
		case session.StateLost:
			return v, inStep("launch", fmt.Errorf("monitor died during launch; see %s", v.Dir.MonitorLog()))
		}
		if time.Now().After(deadline) {
			return v, inStep("launch", fmt.Errorf("session %s did not start within %s; see %s", name, timeout, v.Dir.MonitorLog()))
		}
		time.Sleep(25 * time.Millisecond)
	}
}
