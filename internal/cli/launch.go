package cli

import (
	"fmt"
	"io"
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

// selectFunc picks the launcher. It is a variable only so a test in this
// package can substitute one whose Start fails; production never reassigns it.
var selectFunc = launcher.Select

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
	perms := prof.ResolvePermissions(o.Permissions, cfg.DefaultPermissions)
	r, err := prof.RenderHeadless(profile.Request{Permissions: perms, Model: o.Model, Prompt: prompt})
	if err != nil {
		return nil, inStep("profile", err)
	}
	if err := validateEnv(o.Env); err != nil {
		return nil, inStep("binary", err)
	}
	p := &prepared{cfg: cfg, prof: prof, perms: perms, rendered: r}
	p.envBase = runenv.Base(cfg.EnvPassthrough, os.Environ())
	if p.envBase == nil {
		p.envBase = []string{}
	}
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
	return p, nil
}

// selectLauncher resolves --launcher / config. It belongs to the launch step,
// so `check` only reaches it after the binary step has run.
func (p *prepared) selectLauncher(o launchOptions, warn io.Writer) (launcher.Launcher, error) {
	pref := o.Launcher
	if pref == "" {
		pref = p.cfg.Launcher
	}
	l, err := selectFunc(pref, warn)
	if err != nil {
		return nil, inStep("launch", err)
	}
	return l, nil
}

// maxPromptBytes caps a prompt read from --prompt-file (or stdin).
const maxPromptBytes = 4 << 20

// readPromptFile reads the prompt for --prompt-file. "-" means stdin and is
// recorded as the source "stdin"; anything else must be a regular file or a pipe (never a
// device or directory), and is
// recorded by absolute path. Both are capped at maxPromptBytes.
func readPromptFile(path string, stdin io.Reader) (text, source string, err error) {
	var r io.Reader
	if path == "-" {
		r, source = stdin, "stdin"
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", "", err
		}
		// Stat first (it follows symlinks) so a device is never opened:
		// opening one can have side effects. Regular files and pipes (a FIFO,
		// or process substitution such as <(cmd), /dev/fd/N) are read; opening
		// a FIFO blocks until a writer appears, as usual.
		fi, err := os.Stat(abs)
		if err != nil {
			return "", "", err
		}
		if m := fi.Mode(); !m.IsRegular() && m&os.ModeNamedPipe == 0 {
			return "", "", usagef("--prompt-file %s: not a regular file or pipe (use - to read the prompt from stdin)", path)
		}
		f, err := os.Open(abs)
		if err != nil {
			return "", "", err
		}
		defer f.Close()
		r, source = f, abs
	}
	b, err := io.ReadAll(io.LimitReader(r, maxPromptBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(b) > maxPromptBytes {
		return "", "", usagef("prompt from %s is larger than the %d MiB limit", source, maxPromptBytes>>20)
	}
	return string(b), source, nil
}

// launch creates a session and starts its monitor. On a handshake failure it
// returns the view (if one exists) together with the error.
func launch(o launchOptions, stderr io.Writer) (*session.View, error) {
	if err := validateDurations(o.Grace, o.MaxRuntime); err != nil {
		return nil, err
	}
	if err := validateEnv(o.Env); err != nil {
		return nil, err
	}
	prompt, source := o.Prompt, "inline"
	if o.PromptFile != "" {
		if prompt != "" {
			return nil, usagef("--prompt and --prompt-file are mutually exclusive")
		}
		var err error
		if prompt, source, err = readPromptFile(o.PromptFile, os.Stdin); err != nil {
			return nil, err
		}
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
	l, err := p.selectLauncher(o, stderr)
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
		Marker: session.NewMarker(name, 1), FastenerVersion: version.String(), PlannedAt: now,
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
		os.RemoveAll(string(d))
		return nil, inStep("launch", err)
	}
	for _, w := range writes {
		if err := session.WriteJSON(w.path, w.v); err != nil {
			os.RemoveAll(string(d))
			return nil, inStep("launch", err)
		}
	}
	rec, err := l.Start(d, 1)
	if err != nil {
		_, _ = session.Finalize(d, func(s *session.Status) {
			t := time.Now().UTC()
			s.State, s.EndedBy, s.EndedAt, s.Error = session.StateExited, session.EndedByLaunchFailed, &t, "monitor: "+err.Error()
		})
		// Return the (finalized) view too, so `run --json` can print it.
		nv, lerr := session.LoadFull(name, p.cfg.Handshake())
		if lerr != nil {
			// No view to hand back: do not leave an unreported directory behind.
			os.RemoveAll(string(d))
			nv = nil
		}
		return nv, inStep("launch", fmt.Errorf("session %s: start monitor: %w", name, err))
	}
	if err := session.WriteJSON(d.LauncherPath(1), rec); err != nil {
		// Not fatal: status.json carries the monitor identity once it starts.
		fmt.Fprintf(stderr, "fastener: warning: session %s: cannot write launcher record: %v\n", name, err)
	}
	return handshake(name, p.cfg.Handshake())
}

// handshakeLostAction decides what the handshake does with a session whose
// monitor is gone. It finalizes as launch-failed only when nothing was ever
// spawned: no agent identity, a status still reading starting, and no marked
// process alive (v must have had FindOrphans run). Otherwise the session stays
// lost so stop/rm can see and clean up the agent and its orphans.
func handshakeLostAction(v *session.View) (finalize bool) {
	if v.Status.Agent != nil && v.Status.State != session.StateStarting {
		return false
	}
	return len(v.Orphans) == 0
}

func handshake(name string, timeout time.Duration) (*session.View, error) {
	deadline := time.Now().Add(timeout)
	// full upgrades a cheap view to one with the orphan scan, for error returns.
	full := func(v *session.View) *session.View { return withOrphanScan(v, name, timeout, session.LoadFull) }
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
				return v, inStep("launch", fmt.Errorf("session %s: launch failed: %s", name, v.Status.Error))
			}
			return v, nil // a fast agent may already have finished
		case session.StateLost:
			msg := fmt.Sprintf("monitor died during launch; see %s", v.Dir.MonitorLog())
			v.FindOrphans()
			if handshakeLostAction(v) {
				_, _ = session.Finalize(v.Dir, func(s *session.Status) {
					t := time.Now().UTC()
					s.State, s.EndedBy, s.EndedAt, s.Error = session.StateExited, session.EndedByLaunchFailed, &t, msg
				})
			}
			return full(v), inStep("launch", fmt.Errorf("session %s: %s", name, msg))
		}
		if time.Now().After(deadline) {
			// Never leave a late-starting monitor to run the prompt unattended:
			// the stop request is honoured at monitor start and before the spawn.
			outcome := "the session was stopped"
			if serr := stopSession(v, 0, "stop"); serr != nil {
				outcome = fmt.Sprintf("stopping it failed: %v", serr)
			}
			return full(v), inStep("launch", fmt.Errorf("session %s did not start within %s (%s); see %s", name, timeout, outcome, v.Dir.MonitorLog()))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// withOrphanScan upgrades a cheap view to one with the orphan scan, for error
// returns. It retries load once; if both attempts fail it keeps v but sets
// OrphanErr, so the orphan count is reported as unknown rather than silently low.
func withOrphanScan(v *session.View, name string, timeout time.Duration,
	load func(string, time.Duration) (*session.View, error)) *session.View {
	var err error
	for i := 0; i < 2; i++ {
		var nv *session.View
		if nv, err = load(name, timeout); err == nil {
			return nv
		}
	}
	if v.OrphanErr == "" {
		v.OrphanErr = "orphan scan unavailable: " + err.Error()
	}
	return v
}
