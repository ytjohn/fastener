package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ytjohn/fastener/internal/session"
)

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run <backend>", stderr)
	var o launchOptions
	var env, labels multiFlag
	var follow, asJSON, attachAfter bool
	var sizeFlag string
	fs.StringVar(&o.Name, "name", "", "session name (generated if omitted)")
	fs.StringVar(&o.Cwd, "cwd", "", "working directory for the agent (required)")
	fs.StringVar(&o.Prompt, "prompt", "", "prompt text")
	fs.StringVar(&o.PromptFile, "prompt-file", "", "read the prompt from a file, or - for stdin (copied at launch; max 4 MiB)")
	fs.StringVar(&o.Permissions, "permissions", "", "permission level: auto|skip (default: config, else the profile's default, else auto)")
	fs.StringVar(&o.Model, "model", "", "model passed to the backend")
	fs.StringVar(&o.EnvFile, "env-file", "", "KEY=VALUE file read by the monitor (use for secrets)")
	fs.StringVar(&o.Launcher, "launcher", "", "auto|setsid|systemd (default: config)")
	fs.DurationVar(&o.MaxRuntime, "max-runtime", 0, "stop the agent after this long (0 = no limit)")
	fs.DurationVar(&o.Grace, "grace", 10*time.Second, "time between SIGTERM and SIGKILL")
	fs.Var(&env, "env", "KEY=VALUE for the agent (repeatable; recorded, not for secrets)")
	fs.Var(&labels, "label", "KEY=VALUE label (repeatable)")
	fs.BoolVar(&follow, "follow", false, "follow output after launch")
	fs.BoolVar(&asJSON, "json", false, "print the session as JSON")
	fs.BoolVar(&o.Remote, "remote", false, "start the backend's remote bridge ([remote] in its profile) instead of a headless run; takes no prompt")
	fs.StringVar(&o.Restart, "restart", restartNo, "restart policy: no|on-reboot (on-reboot needs --remote; `fastener restore` relaunches it after a reboot)")
	fs.BoolVar(&o.Interactive, "interactive", false, "run the backend's TUI ([interactive] in its profile) on a pty you can attach to; the prompt is optional; with --remote, also driven remotely ([interactive.remote])")
	fs.StringVar(&sizeFlag, "size", "", "with --interactive: the terminal size as ROWSxCOLS (default: this terminal's size, else 24x80)")
	fs.BoolVar(&attachAfter, "attach", false, "with --interactive: attach this terminal right after the launch, as fastener attach does")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
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
