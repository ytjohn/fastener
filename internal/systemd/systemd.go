// Package systemd talks to the user's systemd manager through systemctl. Every
// call is bounded by a short timeout, so a hung or absent manager never stalls
// the CLI; callers decide what an error means (usually "unknown").
package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProbeTimeout bounds the availability probe that `--launcher auto` runs on every launch.
const ProbeTimeout = time.Second

// queryTimeout bounds unit queries and signals.
const queryTimeout = 3 * time.Second

func systemctl(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), fmt.Errorf("systemctl --user %s: timed out after %s", strings.Join(args, " "), timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

// Available reports whether a usable user manager is running: `systemctl
// --user is-system-running` says running or degraded (it exits non-zero for
// degraded, so the answer is read from stdout), and systemd-run is on PATH.
// The error explains a false answer.
func Available() (bool, error) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false, err
	}
	out, err := systemctl(ProbeTimeout, "is-system-running")
	switch state := strings.TrimSpace(out); state {
	case "running", "degraded":
		return true, nil
	case "":
		if err == nil {
			err = errors.New("systemctl --user is-system-running printed nothing")
		}
		return false, err
	default:
		return false, fmt.Errorf("user manager is %s", state)
	}
}

// UnitAlive reports whether unit is active, activating, deactivating, reloading
// or refreshing. inactive, failed, maintenance and a unit that is not loaded
// (collected after it stopped) are not alive. An error means the state could
// not be determined.
func UnitAlive(unit string) (bool, error) {
	out, err := systemctl(queryTimeout, "show", unit, "-p", "ActiveState", "-p", "LoadState")
	if err != nil {
		return false, err
	}
	props := parseProps(out)
	active, ok := props["ActiveState"]
	if !ok {
		return false, fmt.Errorf("systemctl --user show %s: no ActiveState in %q", unit, strings.TrimSpace(out))
	}
	switch active {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return true, nil
	case "inactive", "failed", "maintenance":
		return false, nil
	}
	return false, fmt.Errorf("unit %s: unrecognised ActiveState %q", unit, active)
}

// MainPID returns the unit's main process id, 0 when it has none.
func MainPID(unit string) (int, error) {
	out, err := systemctl(queryTimeout, "show", unit, "-p", "MainPID")
	if err != nil {
		return 0, err
	}
	v, ok := parseProps(out)["MainPID"]
	if !ok {
		return 0, fmt.Errorf("systemctl --user show %s: no MainPID in %q", unit, strings.TrimSpace(out))
	}
	return strconv.Atoi(v)
}

// KillMain sends SIGTERM to the unit's main process only. It never stops the
// unit: `systemctl stop` would signal the whole cgroup at once and bypass the
// monitor's TERM / grace / KILL sequence. Newer systemd names the option
// --kill-whom (and still accepts --kill-who as an unambiguous prefix of it);
// older systemd only knows --kill-who, so that is the fallback.
func KillMain(unit string) error {
	_, err := systemctl(queryTimeout, "kill", "--kill-whom=main", "--signal=SIGTERM", unit)
	if err != nil && strings.Contains(err.Error(), "unrecognized option") {
		_, err = systemctl(queryTimeout, "kill", "--kill-who=main", "--signal=SIGTERM", unit)
	}
	return err
}

// KillAll sends SIGKILL to every process in the unit's cgroup. Only for a unit
// that should not be running anything yet (a failed launch).
func KillAll(unit string) error {
	_, err := systemctl(queryTimeout, "kill", "--signal=SIGKILL", unit)
	return err
}

// ResetFailed clears a unit's failed state, so it can be collected.
func ResetFailed(unit string) error {
	_, err := systemctl(queryTimeout, "reset-failed", unit)
	return err
}

// Linger reports whether the calling user has lingering enabled (`loginctl
// show-user <uid> -p Linger --value` is yes). Without it the user manager, and
// every unit in it, stops when the user's last session ends.
func Linger() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "Linger", "--value").Output()
	if ctx.Err() != nil {
		return false, fmt.Errorf("loginctl show-user: timed out after %s", ProbeTimeout)
	}
	if err != nil {
		return false, fmt.Errorf("loginctl show-user: %w", err)
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

func parseProps(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}
