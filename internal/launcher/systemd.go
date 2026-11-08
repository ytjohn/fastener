package launcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
	"github.com/ytjohn/fastener/internal/systemd"
)

// Systemd starts the monitor as a transient user service:
//
//	systemd-run --user --unit=fastener-<name>-g<gen>-<rand> --collect --quiet
//	  -p Type=exec -p KillMode=control-group -p TimeoutStopSec=<grace+30s>
//	  [-p RuntimeMaxSec=<max_runtime+grace+30s>] -p WorkingDirectory=/
//	  -p StandardOutput=append:<monitor.log> -p StandardError=append:<monitor.log>
//	  -- <abs fastener> _monitor <abs dir> <gen>
//
// The cgroup holds the whole process tree, so when the monitor exits or dies
// systemd kills whatever is left. The user manager's PATH differs from the
// shell's, so the binary is named by absolute path.
type Systemd struct{}

func (Systemd) Name() string { return "systemd" }

// stopMargin is added to the grace for TimeoutStopSec and RuntimeMaxSec, so
// systemd never cuts the monitor's own TERM / grace / KILL sequence short.
const stopMargin = 30 * time.Second

// runTimeout bounds systemd-run itself (it waits for the start job).
const runTimeout = 30 * time.Second

func (Systemd) Start(d session.Dir, gen int) (session.LauncherRecord, error) {
	exe, err := os.Executable()
	if err != nil {
		return session.LauncherRecord{}, err
	}
	d, args, err := monitorArgs(d, gen)
	if err != nil {
		return session.LauncherRecord{}, err
	}
	var sess session.Session
	if err := session.ReadJSON(d.SessionPath(), &sess); err != nil {
		return session.LauncherRecord{}, err
	}
	// Create the log ourselves, so it gets the same 0600 mode as in setsid mode
	// (the manager would create it 0644).
	logf, err := os.OpenFile(d.MonitorLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return session.LauncherRecord{}, err
	}
	logf.Close()

	unit := unitName(sess.Name, gen)
	argv := systemdRunArgs(unit, sess, d.MonitorLog(), append([]string{exe}, args...))
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run", argv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return session.LauncherRecord{}, fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(out.String()))
	}
	boot, _ := proc.BootID()
	return session.LauncherRecord{
		SchemaVersion: session.SchemaVersion, Launcher: "systemd", Unit: unit,
		Monitor: mainIdentity(unit), BootID: boot, LaunchedAt: time.Now().UTC(),
	}, nil
}

// mainIdentity returns the unit's main process identity, or nil if it cannot
// be had (the record then holds only the unit, which the CLI judges by the
// unit's state). MainPID is read again after the start time, so a pid that
// exited and was reused in between is never recorded.
func mainIdentity(unit string) *proc.Identity {
	pid, err := systemd.MainPID(unit)
	if err != nil || pid <= 0 {
		return nil
	}
	id, err := proc.Of(pid)
	if err != nil {
		return nil
	}
	if again, err := systemd.MainPID(unit); err != nil || again != pid {
		return nil
	}
	return &id
}

// unitName is fastener-<name>-g<gen>-<random>. Session names already match
// [a-z0-9][a-z0-9._-]{0,62}, all valid in unit names, so no escaping is
// needed. The generation keeps a restore distinct; the random suffix keeps two
// state directories (or a recreated name whose old unit is still stopping)
// from colliding.
func unitName(name string, gen int) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return "fastener-" + name + "-g" + strconv.Itoa(gen) + "-" + suffix + ".service"
}

func systemdRunArgs(unit string, sess session.Session, monitorLog string, command []string) []string {
	grace := sess.EffectiveGrace()
	args := []string{
		"--user", "--unit=" + unit, "--collect", "--quiet",
		"-p", "Type=exec",
		"-p", "KillMode=control-group",
		"-p", "TimeoutStopSec=" + seconds(grace+stopMargin),
		"-p", "WorkingDirectory=/",
		"-p", "StandardInput=null",
		"-p", "StandardOutput=append:" + monitorLog,
		"-p", "StandardError=append:" + monitorLog,
	}
	if mr := time.Duration(sess.MaxRuntime); mr > 0 {
		args = append(args, "-p", "RuntimeMaxSec="+seconds(mr+grace+stopMargin))
	}
	args = append(args, "--")
	for _, a := range command {
		// The manager expands ${VAR} in ExecStart; $$ is a literal $.
		args = append(args, strings.ReplaceAll(a, "$", "$$"))
	}
	return args
}

// seconds renders d as whole seconds, rounded up, in systemd's time syntax.
func seconds(d time.Duration) string {
	s := (d + time.Second - 1) / time.Second
	return strconv.FormatInt(int64(s), 10) + "s"
}
