package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ytjohn/fastener/internal/session"
)

// runHelp is `run -h`: the flags grouped by the mode they apply to, which
// Go's flat flag list cannot show. TestRunHelpListsEveryFlag keeps it in
// step with the flag set.
const runHelp = `usage: fastener run <backend> --cwd DIR <mode> [flags]

Launch a detached session of <backend> (see fastener backends) and print its
name.

modes (pick one):
  --prompt TEXT | --prompt-file F   headless run: one prompt, then the agent
                                    exits (--prompt-file - reads stdin)
  --remote                          remote bridge, driven from the backend's
                                    web or mobile app; prints its URL
  --interactive [--remote]          the backend's TUI on a pty; connect with
                                    fastener attach (a prompt is optional);
                                    with --remote, also driven remotely

all modes:
  --cwd DIR             working directory for the agent (required)
  --name NAME           session name (default: generated)
  --model MODEL         model passed to the backend
  --permissions LEVEL   auto|skip (default: config, else the profile's, else auto)
  --env KEY=VALUE       environment for the agent (repeatable; recorded, so
                        not for secrets)
  --env-file FILE       KEY=VALUE file read by the monitor (use for secrets)
  --label KEY=VALUE     label the session (repeatable)
  --max-runtime DUR     stop the agent after this long (default: no limit)
  --grace DUR           time between SIGTERM and SIGKILL (default 10s)
  --launcher NAME       auto|setsid|systemd (default: config)
  --json                print the session as JSON

headless only:
  --follow              follow the output, then exit as fastener wait does

--remote and --interactive:
  --restart POLICY      no|on-reboot (default no); fastener restore relaunches
                        on-reboot sessions after a reboot, an interactive one
                        resuming its conversation

--interactive only:
  --size ROWSxCOLS      pty size (default: this terminal's, else 24x80)
  --attach              attach this terminal right after the launch
                        (not with --json)
`

// runFlags holds run's flag values.
type runFlags struct {
	o                           launchOptions
	env, labels                 multiFlag
	follow, asJSON, attachAfter bool
	size                        string
}

func (r *runFlags) define(fs *flag.FlagSet) {
	fs.StringVar(&r.o.Name, "name", "", "session name (generated if omitted)")
	fs.StringVar(&r.o.Cwd, "cwd", "", "working directory for the agent (required)")
	fs.StringVar(&r.o.Prompt, "prompt", "", "prompt text")
	fs.StringVar(&r.o.PromptFile, "prompt-file", "", "read the prompt from a file, or - for stdin (copied at launch; max 4 MiB)")
	fs.StringVar(&r.o.Permissions, "permissions", "", "permission level: auto|skip (default: config, else the profile's default, else auto)")
	fs.StringVar(&r.o.Model, "model", "", "model passed to the backend")
	fs.StringVar(&r.o.EnvFile, "env-file", "", "KEY=VALUE file read by the monitor (use for secrets)")
	fs.StringVar(&r.o.Launcher, "launcher", "", "auto|setsid|systemd (default: config)")
	fs.DurationVar(&r.o.MaxRuntime, "max-runtime", 0, "stop the agent after this long (0 = no limit)")
	fs.DurationVar(&r.o.Grace, "grace", 10*time.Second, "time between SIGTERM and SIGKILL")
	fs.Var(&r.env, "env", "KEY=VALUE for the agent (repeatable; recorded, not for secrets)")
	fs.Var(&r.labels, "label", "KEY=VALUE label (repeatable)")
	fs.BoolVar(&r.follow, "follow", false, "follow output after launch")
	fs.BoolVar(&r.asJSON, "json", false, "print the session as JSON")
	fs.BoolVar(&r.o.Remote, "remote", false, "start the backend's remote bridge ([remote] in its profile) instead of a headless run; takes no prompt")
	fs.StringVar(&r.o.Restart, "restart", restartNo, "restart policy: no|on-reboot (on-reboot needs --remote or --interactive)")
	fs.BoolVar(&r.o.Interactive, "interactive", false, "run the backend's TUI ([interactive] in its profile) on a pty you can attach to; the prompt is optional; with --remote, also driven remotely ([interactive.remote])")
	fs.StringVar(&r.size, "size", "", "with --interactive: the terminal size as ROWSxCOLS (default: this terminal's size, else 24x80)")
	fs.BoolVar(&r.attachAfter, "attach", false, "with --interactive: attach this terminal right after the launch, as fastener attach does")
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run <backend>", stderr)
	var r runFlags
	r.define(fs)
	fs.Usage = func() { fmt.Fprint(stderr, runHelp) }
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	o, env, labels := r.o, r.env, r.labels
	follow, asJSON, attachAfter, sizeFlag := r.follow, r.asJSON, r.attachAfter, r.size
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: fastener run <backend> --cwd DIR (--prompt TEXT | --prompt-file F | --remote | --interactive [--remote]) [flags]")
		return exitUsage
	}
	o.Backend, o.Env = pos[0], env
	if err := validatePermissions(o.Permissions); err != nil {
		return fail(stderr, err)
	}
	if o.Labels, err = parseLabels(labels); err != nil {
		return fail(stderr, err)
	}
	if err := validateRestart(o.Restart, o.Remote, o.Interactive); err != nil {
		return fail(stderr, err)
	}
	if !o.Interactive && (sizeFlag != "" || attachAfter) {
		fmt.Fprintln(stderr, "fastener: --size and --attach need --interactive")
		return exitUsage
	}
	if o.Interactive {
		switch {
		case follow:
			fmt.Fprintln(stderr, "fastener: --follow cannot be used with --interactive; use --attach (or fastener logs -f <name>)")
			return exitUsage
		case attachAfter && asJSON:
			fmt.Fprintln(stderr, "fastener: --json and --attach cannot be combined")
			return exitUsage
		case attachAfter && !term.IsTerminal(int(os.Stdin.Fd())):
			// Checked before launching, so nothing starts that cannot be attached.
			fmt.Fprintln(stderr, "fastener: --attach needs a terminal on stdin")
			return exitUsage
		}
		size, err := interactiveSize(sizeFlag)
		if err != nil {
			return fail(stderr, err)
		}
		o.PTY = size
	}
	if follow && o.Remote {
		// A bridge runs until stopped, so --follow would never return.
		fmt.Fprintln(stderr, "fastener: --follow cannot be used with --remote (a bridge runs until stopped); use fastener logs -f <name>")
		return exitUsage
	}
	if follow && asJSON {
		fmt.Fprintln(stderr, "fastener: --json and --follow cannot be combined")
		return exitUsage
	}
	autoPrune()
	v, p, err := launchPrepared(o, stderr)
	if err != nil {
		if v != nil && asJSON {
			writeJSON(stdout, inspectOf(v))
		}
		return fail(stderr, err)
	}
	if o.Remote {
		// Wait for the bridge to report its URL; a bridge that ends first is
		// a failure with its reason.
		wait := p.cfg.RemoteWaitDuration()
		v, err = waitRemoteURL(v, p, wait)
		out := inspectOf(v)
		var rf *remoteFailure
		if errors.As(err, &rf) {
			out.Reason, out.Folder = rf.reason, rf.folder
		}
		if asJSON {
			writeJSON(stdout, out)
		}
		if err != nil {
			return fail(stderr, err)
		}
		if !asJSON {
			if u := v.Status.RemoteURL; u != "" {
				fmt.Fprintf(stdout, "%s  %s\n", v.Session.Name, u)
			} else {
				fmt.Fprintln(stdout, v.Session.Name)
			}
		}
		if v.Status.RemoteURL == "" {
			fmt.Fprintf(stderr, "fastener: %s: no URL yet after %s; the %s is still running (fastener inspect %s shows the URL once reported)\n",
				v.Session.Name, wait, remoteNoun(p), v.Session.Name)
		}
	} else if asJSON {
		writeJSON(stdout, inspectOf(v))
	} else if !attachAfter {
		fmt.Fprintln(stdout, v.Session.Name)
	}
	if attachAfter {
		return cmdAttach([]string{v.Session.Name}, stdout, stderr)
	}
	if follow {
		code, fv := followLog(v.Session.Name, v.Dir.OutputLog(), stdout, stderr)
		if fv == nil {
			return code
		}
		// Report the agent's outcome exactly as `wait` would.
		return outcomeCode(v.Session.Name, fv, stderr)
	}
	return exitOK
}

// interactiveSize is --size ROWSxCOLS, else this terminal's size when stdin
// is one, else 24x80.
func interactiveSize(flag string) (session.PTYPlan, error) {
	if flag != "" {
		r, c, ok := strings.Cut(strings.ToLower(flag), "x")
		rows, rerr := strconv.Atoi(r)
		cols, cerr := strconv.Atoi(c)
		if !ok || rerr != nil || cerr != nil || rows < 1 || cols < 1 || rows > 0xffff || cols > 0xffff {
			return session.PTYPlan{}, usagef("--size %q: want ROWSxCOLS, e.g. 40x120", flag)
		}
		return session.PTYPlan{Rows: uint16(rows), Cols: uint16(cols)}, nil
	}
	if in := int(os.Stdin.Fd()); term.IsTerminal(in) {
		if cols, rows, err := term.GetSize(in); err == nil && rows > 0 && cols > 0 {
			return session.PTYPlan{Rows: uint16(rows), Cols: uint16(cols)}, nil
		}
	}
	return session.PTYPlan{Rows: session.DefaultPTYRows, Cols: session.DefaultPTYCols}, nil
}
