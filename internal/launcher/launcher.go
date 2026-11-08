// Package launcher starts a session's monitor detached from the calling CLI.
package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
)

type Launcher interface {
	Name() string
	Start(d session.Dir, gen int) (session.LauncherRecord, error)
}

// Select resolves the launcher preference ("", "auto", "setsid", "systemd").
func Select(pref string) (Launcher, error) {
	switch pref {
	case "", "auto", "setsid":
		return Setsid{}, nil
	case "systemd":
		return nil, errors.New("launcher systemd is not available until v0.2; use auto or setsid")
	}
	return nil, fmt.Errorf("unknown launcher %q (want auto, setsid or systemd)", pref)
}

// Setsid re-executes this binary as `_monitor` in a new session, with stdio
// to monitor.log, and releases it without waiting. Go cannot fork without
// exec, so there is no double fork; the CLI knows the monitor pid directly.
type Setsid struct{}

func (Setsid) Name() string { return "setsid" }

func (Setsid) Start(d session.Dir, gen int) (session.LauncherRecord, error) {
	exe, err := os.Executable()
	if err != nil {
		return session.LauncherRecord{}, err
	}
	logf, err := os.OpenFile(d.MonitorLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return session.LauncherRecord{}, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "_monitor", string(d), strconv.Itoa(gen))
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
