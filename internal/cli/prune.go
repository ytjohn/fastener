package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/session"
)

type pruneOut struct {
	Removed []string               `json:"removed"`
	Kept    []session.PruneKept    `json:"kept"`
	Failed  []session.PruneFailure `json:"failed,omitempty"`
}

func cmdPrune(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("prune", stderr)
	olderThan := fs.Duration("older-than", 0, "prune sessions that ended longer ago than this (default: config retention, 168h)")
	dry := fs.Bool("dry-run", false, "show what would be removed; remove nothing")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: fastener prune [--older-than D] [--dry-run] [--json]")
		return exitUsage
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(stderr, err)
	}
	ret := cfg.RetentionDuration()
	var set bool
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "older-than" })
	if set {
		if *olderThan <= 0 {
			return fail(stderr, usagef("--older-than must be positive, got %s", *olderThan))
		}
		ret = *olderThan
	}
	if session.SessionsDir() == "" {
		return fail(stderr, session.ErrNoStateDir)
	}
	res := session.PruneLocked(session.PruneOptions{
		Retention: ret, Handshake: cfg.Handshake(), DryRun: *dry,
		Warn: func(s string) { fmt.Fprintf(stderr, "fastener: warning: %s\n", s) },
	})
	for _, f := range res.Failed {
		fmt.Fprintf(stderr, "fastener: prune: removing %s: %s\n", f.Name, f.Err)
	}
	if *asJSON {
		writeJSON(stdout, pruneOut{Removed: res.Removed, Kept: res.Kept, Failed: res.Failed})
	} else {
		verb := "removed"
		if *dry {
			verb = "would remove"
		}
		for _, n := range res.Removed {
			fmt.Fprintf(stdout, "%s %s\n", verb, n)
		}
		for _, k := range res.Kept {
			fmt.Fprintf(stdout, "kept %s (%s)\n", k.Name, k.Reason)
		}
	}
	if len(res.Failed) > 0 {
		return exitFail
	}
	return exitOK
}

// autoPrune is the opportunistic prune run by `run` and `ps`: throttled to
// once an hour, disabled by auto_prune = false, and silent on every error.
func autoPrune() {
	cfg, err := config.Load()
	if err != nil || !cfg.AutoPrune {
		return
	}
	session.AutoPrune(session.PruneOptions{Retention: cfg.RetentionDuration(), Handshake: cfg.Handshake()})
}
