package cli

import (
	"fmt"
	"io"
	"time"
)

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run <backend>", stderr)
	var o launchOptions
	var env, labels multiFlag
	var follow, asJSON bool
	fs.StringVar(&o.Name, "name", "", "session name (generated if omitted)")
	fs.StringVar(&o.Cwd, "cwd", "", "working directory for the agent (required)")
	fs.StringVar(&o.Prompt, "prompt", "", "prompt text")
	fs.StringVar(&o.PromptFile, "prompt-file", "", "read the prompt from a file (copied at launch)")
	fs.StringVar(&o.Permissions, "permissions", "", "permission level: auto|skip (default: config, else auto)")
	fs.StringVar(&o.Model, "model", "", "model passed to the backend")
	fs.StringVar(&o.EnvFile, "env-file", "", "KEY=VALUE file read by the monitor (use for secrets)")
	fs.StringVar(&o.Launcher, "launcher", "", "auto|setsid|systemd (default: config)")
	fs.DurationVar(&o.MaxRuntime, "max-runtime", 0, "stop the agent after this long (0 = no limit)")
	fs.DurationVar(&o.Grace, "grace", 10*time.Second, "time between SIGTERM and SIGKILL")
	fs.Var(&env, "env", "KEY=VALUE for the agent (repeatable; recorded, not for secrets)")
	fs.Var(&labels, "label", "KEY=VALUE label (repeatable)")
	fs.BoolVar(&follow, "follow", false, "follow output after launch")
	fs.BoolVar(&asJSON, "json", false, "print the session as JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: fastener run <backend> --cwd DIR (--prompt TEXT | --prompt-file F) [flags]")
		return exitUsage
	}
	o.Backend, o.Env = pos[0], env
	if o.Labels, err = parseLabels(labels); err != nil {
		return fail(stderr, err)
	}
	v, err := launch(o, stderr)
	if err != nil {
		if v != nil && asJSON {
			writeJSON(stdout, inspectOf(v))
		}
		return fail(stderr, err)
	}
	if asJSON {
		writeJSON(stdout, inspectOf(v))
	} else {
		fmt.Fprintln(stdout, v.Session.Name)
	}
	if follow {
		return followLog(v.Session.Name, v.Dir.OutputLog(), stdout, stderr)
	}
	return exitOK
}
