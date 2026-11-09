// Package cli implements the fastener command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"

	"github.com/ytjohn/fastener/internal/monitor"
	"github.com/ytjohn/fastener/internal/profile"
	"github.com/ytjohn/fastener/internal/session"
)

const (
	exitOK          = 0
	exitFail        = 1
	exitUsage       = 2
	exitWaitTimeout = 124
	exitAbnormal    = 125
	exitNotFound    = 127
)

const usageText = `usage: fastener <command> [arguments]

commands:
  run <backend>      launch a headless session or a remote bridge (detached)
  ps                 list sessions
  inspect <name>     show a session's state and facts
  logs <name>        print a session's output
  wait <name>        wait for a session to end; exit with its code
  stop <name>        stop a session
  rm <name>          remove a session's state
  prune              remove old ended sessions
  check <backend>    prove a backend works end to end
  backends           list backend profiles
  authorize          trust a folder for Claude remote bridges (edits Claude's config)
  restore [name...]  relaunch lost --restart on-reboot bridges (after a reboot)
  version            print the version

Run "fastener <command> -h" for a command's flags.
Docs and issues: https://github.com/ytjohn/fastener
`

// Main runs one CLI invocation and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "_monitor":
		return runMonitor(rest, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "ps":
		return cmdPs(rest, stdout, stderr)
	case "inspect":
		return cmdInspect(rest, stdout, stderr)
	case "logs":
		return cmdLogs(rest, stdout, stderr)
	case "wait":
		return cmdWait(rest, stdout, stderr)
	case "stop":
		return cmdStop(rest, stdout, stderr)
	case "rm":
		return cmdRm(rest, stdout, stderr)
	case "prune":
		return cmdPrune(rest, stdout, stderr)
	case "check":
		return cmdCheck(rest, stdout, stderr)
	case "backends":
		return cmdBackends(rest, stdout, stderr)
	case "authorize":
		return cmdAuthorize(rest, stdout, stderr)
	case "restore":
		return cmdRestore(rest, stdout, stderr)
	case "version":
		return cmdVersion(rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	}
	fmt.Fprintf(stderr, "fastener: unknown command %q\n\n%s", cmd, usageText)
	return exitUsage
}

func runMonitor(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		return exitUsage
	}
	gen, err := strconv.Atoi(args[1])
	if err != nil || gen < 1 {
		return exitUsage
	}
	return monitor.Run(args[0], gen, log.New(stderr, "fastener-monitor: ", log.LstdFlags|log.LUTC))
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// fail prints err and maps it to an exit code.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "fastener: %v\n", err)
	var ue usageError
	switch {
	case errors.As(err, &ue):
		return exitUsage
	case errors.Is(err, profile.ErrUnknownBackend), errors.Is(err, session.ErrNotFound):
		return exitNotFound
	}
	return exitFail
}
