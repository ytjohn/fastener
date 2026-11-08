package cli

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"
)

func cmdLogs(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("logs <name>", stderr)
	follow := flags.Bool("f", false, "follow until the session ends")
	useStderr := flags.Bool("stderr", false, "show the agent's stderr instead of stdout")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return exitUsage
	}
	name, ok := oneName(pos, stderr, "logs <name> [-f] [--stderr]")
	if !ok {
		return exitUsage
	}
	v, err := loadView(name)
	if err != nil {
		return fail(stderr, err)
	}
	path := v.Dir.OutputLog()
	if *useStderr {
		path = v.Dir.StderrLog()
	}
	if *follow {
		return followLog(name, path, stdout, stderr)
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

// followLog copies path to stdout until the session is no longer alive.
func followLog(name, path string, stdout, stderr io.Writer) int {
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
			return fail(stderr, err)
		}
		if !v.Alive() {
			if f != nil {
				_, _ = io.Copy(stdout, f)
			}
			return exitOK
		}
		time.Sleep(200 * time.Millisecond)
	}
}
