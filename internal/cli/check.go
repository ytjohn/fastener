package cli

import (
	"context"
	"errors"
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

type checkReport struct {
	Backend string      `json:"backend"`
	Passed  bool        `json:"passed"`
	Session string      `json:"session,omitempty"`
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
	fs.StringVar(&o.Permissions, "permissions", "", "permission level (default: config, else auto)")
	fs.StringVar(&o.Model, "model", "", "model passed to the backend")
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
	if err := validateDurations(o.Grace, o.MaxRuntime); err != nil {
		return fail(stderr, err)
	}
	if err := validateEnv(o.Env); err != nil {
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
	for _, b := range backends {
		bo := o
		bo.Backend = b
		r := runCheck(bo, static, keep, stderr)
		if !r.Passed {
			code = exitFail
			if r.unknown && !all {
				code = exitNotFound
			}
		}
		reports = append(reports, r)
		if !asJSON {
			printCheck(stdout, r)
		}
	}
	if asJSON {
		writeJSON(stdout, reports)
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
	if !r.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "%s %s\n\n", verdict, r.Backend)
}

// runCheck uses a named result so the deferred cleanup step lands in the
// returned report (a deferred call runs after `return r` copies a plain result).
func runCheck(o launchOptions, static, keep bool, stderr io.Writer) (r checkReport) {
	r = checkReport{Backend: o.Backend}

	p, err := prepare(o, checkPrompt)
	if err != nil {
		step := "profile"
		var se *stepError
		if errors.As(err, &se) {
			step = se.step
		}
		r.unknown = errors.Is(err, profile.ErrUnknownBackend)
		for _, s := range []string{"profile", "binary"} {
			if s == step {
				break
			}
			r.add(s, stepPass, "", "")
		}
		r.add(step, stepFail, err.Error(), "")
		return r
	}
	r.add("profile", stepPass, fmt.Sprintf("%s, permissions=%s", p.prof.Source, p.perms), "")

	detail := p.argv0
	if va := p.prof.Check.VersionArgs; len(va) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, p.argv0, va...)
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
	if static {
		r.Passed = true
		return r
	}

	if o.Cwd == "" {
		tmp, err := os.MkdirTemp("", "fastener-check-")
		if err != nil {
			r.add("launch", stepFail, err.Error(), "")
			return r
		}
		defer os.RemoveAll(tmp)
		o.Cwd = tmp
	}
	o.Prompt = checkPrompt
	o.Labels = map[string]string{"fastener.check": "true"}

	v, err := launch(o, stderr)
	if v != nil {
		r.Session = v.Session.Name
		defer r.cleanup(v, keep)
	}
	if err != nil {
		hint, more := "", ""
		if v != nil {
			hint, more = diagnose(p.prof, v)
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
		hint, more := diagnose(p.prof, v)
		r.add("exit", stepFail, joinDetail(describeEnd(v), more), hint)
		return r
	}
	r.add("exit", stepPass, "exit 0", "")

	if p.rendered.Parser == "none" {
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
			problems = append(problems, "agent reported an error")
		}
		if s.ParseErrors > 0 {
			problems = append(problems, fmt.Sprintf("%d unparseable lines", s.ParseErrors))
		}
		if len(problems) > 0 {
			hint, _ := diagnose(p.prof, v)
			r.add("parse", stepFail, strings.Join(problems, "; "), hint)
			return r
		}
		r.add("parse", stepPass, "conversation "+s.ConversationID, "")
	}

	text := s.FinalMessage
	if p.rendered.Parser == "none" {
		b, _ := os.ReadFile(v.Dir.OutputLog())
		text = string(b)
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		hint, _ := diagnose(p.prof, v)
		r.add("reply", stepFail, fmt.Sprintf("reply did not contain \"pong\": %q", truncate(strings.TrimSpace(text), 120)), hint)
		return r
	}
	r.add("reply", stepPass, "pong", "")
	r.Passed = true
	return r
}

// cleanup is deferred, so it always runs and is always the last step.
func (r *checkReport) cleanup(v *session.View, keep bool) {
	if cur, err := loadView(v.Session.Name); err == nil && (cur.Alive() || len(cur.Orphans) > 0) {
		if err := stopSession(cur, 0, "rm"); err != nil {
			r.add("cleanup", stepFail, err.Error(), "")
			r.Passed = false
			return
		}
	}
	if keep {
		r.add("cleanup", stepPass, "kept "+v.Session.Name, "")
		return
	}
	if err := os.RemoveAll(string(v.Dir)); err != nil {
		r.add("cleanup", stepFail, err.Error(), "")
		r.Passed = false
		return
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

// diagnose matches the profile's patterns against everything the agent said.
// It returns a hint, or, when nothing matches, the stderr tail as extra detail.
func diagnose(p *profile.Profile, v *session.View) (hint, detail string) {
	errText, _ := os.ReadFile(v.Dir.StderrLog())
	outText, _ := os.ReadFile(v.Dir.OutputLog())
	corpus := []string{string(errText), string(outText), v.Status.FinalMessage, v.Status.Error}
	for _, d := range p.Check.Diagnose {
		re := regexp.MustCompile(d.Match) // validated at load
		for _, c := range corpus {
			if c != "" && re.MatchString(c) {
				return d.Hint, ""
			}
		}
	}
	if v.Status.EndedBy == session.EndedByLimit {
		return timeoutHint, ""
	}
	if tail := lastLines(string(errText), 20); tail != "" {
		return "", "stderr:\n" + tail
	}
	return "", ""
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
