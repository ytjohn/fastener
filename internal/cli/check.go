package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/profile"
	"github.com/ytjohn/fastener/internal/session"
)

const (
	checkPrompt = "Reply with exactly one word: pong"
	timeoutHint = "timed out: the agent may be waiting for interactive input (for example a login prompt)"
	stepPass    = "pass"
	stepFail    = "fail"
	stepSkip    = "skip"
)

type checkStep struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

type checkOpts struct {
	static, keep, all bool
	expectModel       string // --expect-model; "" means use --model
}

type checkReport struct {
	Backend string      `json:"backend"`
	Passed  bool        `json:"passed"`
	Session string      `json:"session,omitempty"`
	Skipped bool        `json:"skipped,omitempty"`
	Steps   []checkStep `json:"steps"`
	unknown bool
}

func (r *checkReport) add(name, status, detail, hint string) {
	r.Steps = append(r.Steps, checkStep{Name: name, Status: status, Detail: detail, Hint: hint})
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("check <backend>", stderr)
	var o launchOptions
	var env multiFlag
	var all, static, keep, asJSON bool
	fs.BoolVar(&all, "all", false, "check every resolvable backend")
	fs.BoolVar(&static, "static", false, "profile and binary checks only (no model call)")
	fs.BoolVar(&keep, "keep", false, "keep the check session for inspection")
	fs.BoolVar(&asJSON, "json", false, "print JSON")
	fs.StringVar(&o.Cwd, "cwd", "", "working directory (default: a fresh temporary directory)")
	fs.StringVar(&o.Permissions, "permissions", "", "permission level (default: config, else the profile's default, else auto)")
	fs.StringVar(&o.Model, "model", "", "model passed to the backend")
	var expectModel string
	fs.StringVar(&expectModel, "expect-model", "", "model the backend must report serving (default: --model)")
	fs.StringVar(&o.EnvFile, "env-file", "", "KEY=VALUE file for the agent")
	fs.StringVar(&o.Launcher, "launcher", "", "auto|setsid|systemd")
	fs.DurationVar(&o.MaxRuntime, "max-runtime", 2*time.Minute, "limit for the check run")
	fs.DurationVar(&o.Grace, "grace", 5*time.Second, "time between SIGTERM and SIGKILL")
	fs.Var(&env, "env", "KEY=VALUE for the agent (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	o.Env = env
	expectSet := false
	fs.Visit(func(f *flag.Flag) { expectSet = expectSet || f.Name == "expect-model" })
	if expected := expectModel; expectSet || o.Model != "" {
		if !expectSet {
			expected = o.Model
		}
		if err := validExpectation(expected); err != nil {
			return fail(stderr, err)
		}
	}
	if static && expectModel != "" {
		fmt.Fprintln(stderr, "fastener: warning: --expect-model has no effect with --static (no model call)")
	}
	if err := validateDurations(o.Grace, o.MaxRuntime); err != nil {
		return fail(stderr, err)
	}
	if err := validateEnv(o.Env); err != nil {
		return fail(stderr, err)
	}
	if err := validatePermissions(o.Permissions); err != nil {
		return fail(stderr, err)
	}
	var backends []string
	switch {
	case all && len(pos) == 0:
		if backends, err = profile.Names(config.BackendsDir()); err != nil {
			return fail(stderr, err)
		}
	case !all && len(pos) == 1:
		backends = pos
	default:
		fmt.Fprintln(stderr, "usage: fastener check <backend> | --all [--static] [--keep] [flags]")
		return exitUsage
	}

	reports := []checkReport{}
	code := exitOK
	checked := 0
	for _, b := range backends {
		bo := o
		bo.Backend = b
		r := runCheck(bo, checkOpts{static: static, keep: keep, all: all, expectModel: expectModel}, stderr)
		if !r.Passed && !r.Skipped {
			code = exitFail
			if r.unknown && !all {
				code = exitNotFound
			}
		}
		if !r.Skipped {
			checked++
		}
		reports = append(reports, r)
		if !asJSON {
			printCheck(stdout, r)
		}
	}
	if asJSON {
		writeJSON(stdout, reports)
	}
	if all && checked == 0 {
		fmt.Fprintln(stderr, "fastener: check --all: no backend was checked (all skipped or none found); pass --permissions or define a backend")
		return exitFail
	}
	return code
}

func printCheck(w io.Writer, r checkReport) {
	fmt.Fprintf(w, "fastener check %s\n", r.Backend)
	for _, s := range r.Steps {
		fmt.Fprintf(w, "  %-4s  %-8s %s\n", strings.ToUpper(s.Status), s.Name, s.Detail)
		if s.Hint != "" {
			fmt.Fprintf(w, "        hint: %s\n", s.Hint)
		}
	}
	verdict := "PASS"
	if r.Skipped {
		verdict = "SKIP"
	} else if !r.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "%s %s\n\n", verdict, r.Backend)
}

// runCheck uses a named result so the deferred cleanup step lands in the
// returned report (a deferred call runs after `return r` copies a plain result).
func runCheck(o launchOptions, opts checkOpts, stderr io.Writer) (r checkReport) {
	r = checkReport{Backend: o.Backend}

	p, err := prepare(o, checkPrompt)
	if err != nil {
		step := "profile"
		var se *stepError
		if errors.As(err, &se) {
			step = se.step
		}
		r.unknown = errors.Is(err, profile.ErrUnknownBackend)
		if opts.all && errors.Is(err, profile.ErrNoPermissionLevel) {
			// Not a failure under --all: the level was never requested for this
			// backend. prepare got this far, so the config and profile load.
			level := o.Permissions
			cfg, cerr := config.Load()
			prof, perr := profile.Load(o.Backend, config.BackendsDir())
			if cerr == nil && perr == nil {
				level = prof.ResolvePermissions(o.Permissions, cfg.DefaultPermissions)
			}
			r.Skipped = true
			r.add("profile", stepSkip, fmt.Sprintf("does not define permission level %s; pass --permissions to check it", level), "")
			return r
		}
		for _, s := range []string{"profile", "binary"} {
			if s == step {
				break
			}
			r.add(s, stepPass, "", "")
		}
		r.add(step, stepFail, err.Error(), "")
		return r
	}
	if opts.static {
		// --static never reaches launch, so validate the launcher name here.
		if _, err := p.selectLauncher(o); err != nil {
			r.add("profile", stepFail, err.Error(), "")
			return r
		}
	}
	r.add("profile", stepPass, fmt.Sprintf("%s, permissions=%s", p.prof.Source, p.perms), "")

	detail := p.argv0
	if va := p.prof.Check.VersionArgs; len(va) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, p.argv0, va...)
		cmd.WaitDelay = 2 * time.Second
		cmd.Env = p.mergedEnv(o.Env)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			r.add("binary", stepFail, fmt.Sprintf("%s %s: %v: %s", p.argv0, strings.Join(va, " "), err, lastLines(string(out), 5)), "")
			return r
		}
		detail += " (" + firstLine(string(out)) + ")"
	}
	r.add("binary", stepPass, detail, "")
	if opts.static {
		r.Passed = true
		return r
	}

	// The session and its temporary cwd are cleaned up by one deferred call,
	// which is also the last step of the report.
	var tmp string
	var created *session.View
	defer func() { r.cleanup(created, opts.keep, tmp) }()
	if o.Cwd == "" {
		var err error
		if tmp, err = os.MkdirTemp("", "fastener-check-"); err != nil {
			r.add("launch", stepFail, err.Error(), "")
			return r
		}
		o.Cwd = tmp
	}
	o.Prompt = checkPrompt
	o.Labels = map[string]string{"fastener.check": "true"}
	parserNone := p.rendered.Parser == "none"

	v, err := launch(o, stderr)
	if v != nil {
		r.Session = v.Session.Name
		created = v
	}
	if err != nil {
		hint, more := "", ""
		if v != nil {
			hint, more = diagnose(p.prof, v, parserNone)
		}
		r.add("launch", stepFail, joinDetail(err.Error(), more), hint)
		return r
	}
	if missing := missingFiles(v.Dir); len(missing) > 0 {
		r.add("launch", stepFail, "missing: "+strings.Join(missing, ", "), "")
		return r
	}
	r.add("launch", stepPass, "session "+v.Session.Name, "")

	v, err = waitEnded(v.Session.Name, o.MaxRuntime+o.Grace+30*time.Second)
	if err != nil {
		r.add("exit", stepFail, err.Error(), "")
		return r
	}
	s := v.Status
	if s.EndedBy != session.EndedByExit || s.ExitCode == nil || *s.ExitCode != 0 {
		hint, more := diagnose(p.prof, v, parserNone)
		r.add("exit", stepFail, joinDetail(describeEnd(v), more), hint)
		return r
	}
	r.add("exit", stepPass, "exit 0", "")

	if parserNone {
		r.add("parse", stepSkip, "backend has no parser", "")
	} else {
		var problems []string
		if s.ConversationID == "" {
			problems = append(problems, "no conversation_id")
		}
		if s.FinalMessage == "" {
			problems = append(problems, "no final_message")
		}
		if s.AgentError != nil && *s.AgentError {
			msg := "agent reported an error"
			if t := strings.TrimSpace(s.FinalMessage); t != "" {
				msg += ": " + truncate(t, 200)
			}
			problems = append(problems, msg)
		}
		if s.ParseErrors > 0 {
			problems = append(problems, fmt.Sprintf("%d unparseable lines", s.ParseErrors))
		}
		if len(problems) > 0 {
			hint, more := diagnose(p.prof, v, parserNone)
			r.add("parse", stepFail, joinDetail(strings.Join(problems, "; "), more), hint)
			return r
		}
		r.add("parse", stepPass, "conversation "+s.ConversationID, "")
	}

	expected, from := o.Model, "--model"
	if opts.expectModel != "" {
		expected, from = opts.expectModel, "--expect-model"
	}
	if expected == "" {
		from = ""
	}
	ms := modelStep(expected, from, s.Model, s.ModelSource)
	r.Steps = append(r.Steps, ms)
	if ms.Status == stepFail {
		return r
	}

	text := s.FinalMessage
	if parserNone {
		b, _ := os.ReadFile(v.Dir.OutputLog())
		text = withoutPromptEcho(string(b), checkPrompt)
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		hint, more := diagnose(p.prof, v, parserNone)
		r.add("reply", stepFail, joinDetail(fmt.Sprintf("reply did not contain \"pong\": %q", truncate(strings.TrimSpace(text), 120)), more), hint)
		return r
	}
	r.add("reply", stepPass, "pong", "")
	r.Passed = true
	return r
}

// withoutPromptEcho drops every line containing the prompt text, so an agent
// that merely echoes its input (cat, a verbose wrapper) cannot pass the reply step.
func withoutPromptEcho(out, prompt string) string {
	p := strings.ToLower(prompt)
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(strings.ToLower(line), p) {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// cleanup is deferred, so it always runs and is always the last step. v is nil
// when launch produced no session; tmp is the temporary cwd ("" if the caller
// supplied one).
func (r *checkReport) cleanup(v *session.View, keep bool, tmp string) {
	if v == nil {
		if tmp != "" {
			os.RemoveAll(tmp)
		}
		return
	}
	fail := func(detail string) {
		r.add("cleanup", stepFail, detail, "")
		r.Passed = false
	}
	if cur, err := loadView(v.Session.Name); err == nil && (cur.Alive() || len(cur.Orphans) > 0) {
		if err := stopSession(cur, 0, "rm"); err != nil {
			detail := "session " + v.Session.Name + " left in place: " + err.Error()
			if tmp != "" {
				detail += "; temporary cwd kept at " + tmp
			}
			fail(detail)
			return
		}
	}
	if keep {
		detail := "kept " + v.Session.Name
		if tmp != "" {
			detail += "; cwd " + tmp
		}
		r.add("cleanup", stepPass, detail, "")
		return
	}
	if err := os.RemoveAll(string(v.Dir)); err != nil {
		detail := err.Error()
		if tmp != "" {
			detail += "; temporary cwd kept at " + tmp
		}
		fail(detail)
		return
	}
	if tmp != "" {
		os.RemoveAll(tmp)
	}
	r.add("cleanup", stepPass, "removed", "")
}

func waitEnded(name string, timeout time.Duration) (*session.View, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := loadView(name)
		if err != nil {
			return nil, err
		}
		if !v.Alive() {
			return v, nil
		}
		if time.Now().After(deadline) {
			_ = stopSession(v, 0, "stop")
			return nil, fmt.Errorf("session did not end within %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func missingFiles(d session.Dir) []string {
	var missing []string
	for _, p := range []string{d.SessionPath(), d.PromptPath(), d.PlanPath(1), d.LauncherPath(1), d.StatusPath(), d.OutputLog()} {
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, p)
		}
	}
	return missing
}

func describeEnd(v *session.View) string {
	s := v.Status
	switch {
	case v.State == session.StateLost:
		return "monitor lost"
	case s.EndedBy == session.EndedByLimit:
		return "killed at --max-runtime"
	case s.Signal != "":
		return fmt.Sprintf("ended_by=%s, killed by %s", s.EndedBy, s.Signal)
	case s.ExitCode != nil:
		return fmt.Sprintf("ended_by=%s, exit code %d", s.EndedBy, *s.ExitCode)
	}
	return "ended_by=" + s.EndedBy
}

// diagnose picks a hint for a failed run. The corpus depends on the source, so
// a model's ordinary reply can never trigger one:
//   - stderr.log and status.error: always;
//   - final_message: only when the agent reported an error, or the parser is none;
//   - output.log: only when the parser is none.
//
// A limit ending gets the timeout hint before any profile pattern. The stderr
// tail (last 20 lines) is returned as detail whenever there is one.
func diagnose(p *profile.Profile, v *session.View, parserNone bool) (hint, detail string) {
	errText, _ := os.ReadFile(v.Dir.StderrLog())
	if tail := lastLines(string(errText), 20); tail != "" {
		detail = "stderr:\n" + tail
	}
	if v.Status.EndedBy == session.EndedByLimit {
		return timeoutHint, detail
	}
	corpus := []string{string(errText), v.Status.Error}
	if parserNone || (v.Status.AgentError != nil && *v.Status.AgentError) {
		corpus = append(corpus, v.Status.FinalMessage)
	}
	if parserNone {
		outText, _ := os.ReadFile(v.Dir.OutputLog())
		corpus = append(corpus, string(outText))
	}
	for _, d := range p.Check.Diagnose {
		re := regexp.MustCompile(d.Match) // validated at load
		for _, c := range corpus {
			if c != "" && re.MatchString(c) {
				return d.Hint, detail
			}
		}
	}
	return "", detail
}

func joinDetail(a, b string) string {
	if b == "" {
		return a
	}
	return a + "\n" + b
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
