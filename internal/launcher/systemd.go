package launcher

import (
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
	"golang.org/x/sys/unix"
)

// Systemd starts the monitor as a transient user service:
//
//	systemd-run --user --unit=fastener-<name>-g<gen>-<rand> --collect --quiet
//	  -p Type=exec -p KillMode=mixed -p TimeoutStopSec=<grace+30s>
//	  [-p RuntimeMaxSec=<max_runtime+grace+30s>] -p LimitNOFILE=<soft>:<hard>
//	  -p WorkingDirectory=/ -p StandardInput=null
//	  -p StandardOutput=append:<monitor.log> -p StandardError=append:<monitor.log>
//	  -- <abs fastener> _monitor <abs dir> <gen>
//
// KillMode=mixed: when systemd stops the unit (the RuntimeMaxSec backstop,
// logout without linger), only the monitor gets SIGTERM and runs its own
// TERM / grace / KILL of the agent; whatever is left in the cgroup once the
// monitor has gone (after a normal exit, or a killed monitor) gets SIGKILL at
// once. LimitNOFILE carries the caller's open-file limit, since the user
// manager's default soft limit (1024) is far below a typical shell's. The
// user manager's PATH differs from the shell's, so the binary is named by
// absolute path.
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
	argv := systemdRunArgs(unit, sess, d.MonitorLog(), nofileLimit(), append([]string{exe}, args...))
	if out, err := runSystemdRun(argv); err != nil {
		return failedStart(unit, fmt.Errorf("systemd-run (unit %s): %w: %s", unit, err, strings.TrimSpace(string(out))))
	}
	boot, _ := proc.BootID()
	return session.LauncherRecord{
		SchemaVersion: session.SchemaVersion, Launcher: "systemd", Unit: unit,
		Monitor: mainIdentity(unit), BootID: boot, LaunchedAt: time.Now().UTC(),
	}, nil
}

// The systemd calls Start makes, replaced in tests.
var (
	runSystemdRun = func(argv []string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "systemd-run", argv...).CombinedOutput()
		if ctx.Err() != nil {
			return out, fmt.Errorf("timed out after %s", runTimeout)
		}
		return out, err
	}
	unitAlive       = systemd.UnitAlive
	killUnit        = systemd.KillAll
	resetFailedUnit = systemd.ResetFailed
)

// failedStart handles a systemd-run that failed or timed out: the manager may
// still have accepted the job. If the unit is alive (or its state is unknown),
// it is killed outright (nothing should be running in it yet, so a whole-cgroup
// SIGKILL is right) and its failed state reset, and err is returned. If that
// kill fails, a unit-only record is returned instead of the error, so the
// handshake and stop can still find and manage the unit.
func failedStart(unit string, err error) (session.LauncherRecord, error) {
	if alive, uerr := unitAlive(unit); !alive && uerr == nil {
		return session.LauncherRecord{}, err
	}
	if kerr := killUnit(unit); kerr != nil {
		boot, _ := proc.BootID()
		return session.LauncherRecord{
			SchemaVersion: session.SchemaVersion, Launcher: "systemd", Unit: unit,
			BootID: boot, LaunchedAt: time.Now().UTC(),
		}, nil
	}
	_ = resetFailedUnit(unit)
	return session.LauncherRecord{}, fmt.Errorf("%w (the unit was started anyway and has been killed)", err)
}

// nofileLimit returns the open-file limit a setsid-launched agent would get,
// as systemd's "<soft>:<hard>". Go raises its own soft limit to hard-1 at
// startup and restores the original in every child, so when Getrlimit shows
// that raise the original soft limit is read from a child (`ulimit -Sn`). It
// returns "" when the limit cannot be read (the unit then gets the manager's
// default).
func nofileLimit() string {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return ""
	}
	soft := lim.Cur
	if lim.Max > 0 && lim.Cur == lim.Max-1 { // Go's own raise; see syscall/rlimit.go
		if orig, ok := childSoftNofile(); ok {
			soft = orig
		}
	}
	return rlimitString(soft) + ":" + rlimitString(lim.Max)
}

// childSoftNofile is the soft RLIMIT_NOFILE a child of this process starts with.
var childSoftNofile = func() (uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", "ulimit -Sn").Output()
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(out))
	if v == "unlimited" {
		return unix.RLIM_INFINITY, true
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, err == nil
}

func rlimitString(v uint64) string {
	if v == unix.RLIM_INFINITY {
		return "infinity"
	}
	return strconv.FormatUint(v, 10)
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

func systemdRunArgs(unit string, sess session.Session, monitorLog, nofile string, command []string) []string {
	grace := sess.EffectiveGrace()
	args := []string{
		"--user", "--unit=" + unit, "--collect", "--quiet",
		"-p", "Type=exec",
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec=" + seconds(grace+stopMargin),
		"-p", "WorkingDirectory=/",
		"-p", "StandardInput=null",
		"-p", "StandardOutput=append:" + monitorLog,
		"-p", "StandardError=append:" + monitorLog,
	}
	if mr := time.Duration(sess.MaxRuntime); mr > 0 {
		args = append(args, "-p", "RuntimeMaxSec="+seconds(mr+grace+stopMargin))
	}
	if nofile != "" {
		args = append(args, "-p", "LimitNOFILE="+nofile)
	}
	args = append(args, "--")
	for _, a := range command {
		// The manager expands both $VAR and ${VAR} in the command; $$ is a literal $.
		args = append(args, strings.ReplaceAll(a, "$", "$$"))
	}
	return args
}

// seconds renders d as whole seconds, rounded up, in systemd's time syntax.
func seconds(d time.Duration) string {
	s := (d + time.Second - 1) / time.Second
	return strconv.FormatInt(int64(s), 10) + "s"
}
