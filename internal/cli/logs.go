package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

func cmdLogs(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("logs <name>", stderr)
	follow := flags.Bool("f", false, "follow until the session ends")
	useStderr := flags.Bool("stderr", false, "show the agent's stderr instead of stdout")
	gen := flags.Int("generation", 0, "show generation N's log (a restored session keeps earlier ones; default: the current generation)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return parseFail(err)
	}
	name, ok := oneName(pos, stderr, "logs <name> [-f] [--stderr] [--generation N]")
	if !ok {
		return exitUsage
	}
	if *gen < 0 {
		return fail(stderr, usagef("--generation must be positive, got %d", *gen))
	}
	v, err := loadView(name)
	if err != nil {
		return fail(stderr, err)
	}
	path := v.Dir.OutputLog()
	if *useStderr {
		path = v.Dir.StderrLog()
	}
	if cur := v.Generation(); *gen > 0 && *gen != cur {
		if *gen > cur {
			return fail(stderr, fmt.Errorf("session %s has no generation %d (the current one is %d)", name, *gen, cur))
		}
		if *follow {
			return fail(stderr, usagef("-f follows the current generation only (%d)", cur))
		}
		path = v.Dir.OutputLogGen(*gen)
		if *useStderr {
			path = v.Dir.StderrLogGen(*gen)
		}
	}
	if *follow {
		code, _ := followLog(name, path, stdout, stderr)
		return code
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return exitOK // nothing written yet
	}
	if err != nil {
		return fail(stderr, err)
	}
	defer f.Close()
	_, _ = io.Copy(stdout, f)
	return exitOK
}

// followLog copies path to stdout until the session is no longer alive. It
// returns the final view (nil on error) so callers can report the outcome.
func followLog(name, path string, stdout, stderr io.Writer) (int, *session.View) {
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for {
		if f == nil {
			if g, err := os.Open(path); err == nil {
				f = g
			}
		}
		if f != nil {
			_, _ = io.Copy(stdout, f)
		}
		v, err := loadView(name)
		if err != nil {
			if errors.Is(err, session.ErrNotFound) {
				// The caller loaded the session before following, so it existed.
				fmt.Fprintf(stderr, "fastener: session %s was removed while following\n", name)
				return exitAbnormal, nil
			}
			return fail(stderr, err), nil
		}
		if !v.Alive() {
			if f == nil {
				if g, err := os.Open(path); err == nil {
					f = g
				}
			}
			if f != nil {
				_, _ = io.Copy(stdout, f)
			}
			return exitOK, v
		}
		time.Sleep(200 * time.Millisecond)
	}
}
