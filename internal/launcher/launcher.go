// Package launcher starts a session's monitor detached from the calling CLI.
package launcher

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
	"github.com/ytjohn/fastener/internal/systemd"
)

type Launcher interface {
	Name() string
	Start(d session.Dir, gen int) (session.LauncherRecord, error)
}

// available and linger probe the user systemd manager (systemd.Available and
// systemd.Linger in production; replaced in tests).
var (
	available = systemd.Available
	linger    = systemd.Linger
	// availableBooting also accepts a manager that is still starting (boot).
	availableBooting = systemd.AvailableOrStarting
)

// Select resolves the launcher preference ("", "auto", "setsid", "systemd").
//
// auto uses systemd only when a user manager is running and the user has
// lingering enabled (a session must not stop at logout just because systemd
// happened to be there); otherwise setsid. Each probe is one exec with a 1 s
// timeout. An explicit systemd is an error without a user manager, and works
// without linger but warns on warn that sessions will stop at logout.
func Select(pref string, warn io.Writer) (Launcher, error) {
	return selectWith(pref, warn, available)
}

// SelectRestore is Select for `fastener restore`, which runs at boot while
// the user manager may still be starting: a manager whose state is
// "starting" counts as usable (it accepts transient units then), so neither
// an explicit systemd launcher fails nor auto silently falls back to setsid.
func SelectRestore(pref string, warn io.Writer) (Launcher, error) {
	return selectWith(pref, warn, availableBooting)
}

func selectWith(pref string, warn io.Writer, available func() (bool, error)) (Launcher, error) {
	switch pref {
	case "", "auto":
		if ok, _ := available(); ok {
			if lingers, _ := linger(); lingers {
				return Systemd{}, nil
			}
		}
		return Setsid{}, nil
	case "setsid":
		return Setsid{}, nil
	case "systemd":
		if ok, err := available(); !ok {
			return nil, fmt.Errorf("launcher systemd: no usable user systemd manager (%v); use auto or setsid", err)
		}
		if lingers, err := linger(); !lingers {
			why := "lingering is off"
			if err != nil {
				why = "lingering could not be checked: " + err.Error()
			}
			fmt.Fprintf(warn, "fastener: warning: launcher systemd: %s, so sessions stop when you log out (loginctl enable-linger keeps them running)\n", why)
		}
		return Systemd{}, nil
	}
	return nil, fmt.Errorf("unknown launcher %q (want auto, setsid or systemd)", pref)
}

// Setsid re-executes this binary as `_monitor` in a new session, with stdio
// to monitor.log, and releases it without waiting. Go cannot fork without
// exec, so there is no double fork; the CLI knows the monitor pid directly.
type Setsid struct {
	// Exe is the fastener binary to run; "" means this one (os.Executable).
	// Tests that run in a test binary set it.
	Exe string
}

func (Setsid) Name() string { return "setsid" }

// executable is exe, else the running binary.
func executable(exe string) (string, error) {
	if exe != "" {
		return exe, nil
	}
	return os.Executable()
}

func (l Setsid) Start(d session.Dir, gen int) (session.LauncherRecord, error) {
	exe, err := executable(l.Exe)
	if err != nil {
		return session.LauncherRecord{}, err
	}
	d, args, err := monitorArgs(d, gen)
	if err != nil {
		return session.LauncherRecord{}, err
	}
	logf, err := os.OpenFile(d.MonitorLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return session.LauncherRecord{}, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, args...)
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return session.LauncherRecord{}, err
	}
	id, err := proc.Of(cmd.Process.Pid)
	if err != nil {
		id = proc.Identity{PID: cmd.Process.Pid} // already gone; its status says why
	}
	boot, _ := proc.BootID()
	_ = cmd.Process.Release()
	return session.LauncherRecord{
		SchemaVersion: session.SchemaVersion, Launcher: "setsid",
		Monitor: &id, BootID: boot, LaunchedAt: time.Now().UTC(),
	}, nil
}

// monitorArgs returns the absolute session dir and the monitor's argv (after
// the executable). The monitor runs with cwd "/", so the dir must be absolute.
func monitorArgs(d session.Dir, gen int) (session.Dir, []string, error) {
	abs, err := filepath.Abs(string(d))
	if err != nil {
		return d, nil, err
	}
	return session.Dir(abs), []string{"_monitor", abs, strconv.Itoa(gen)}, nil
}
