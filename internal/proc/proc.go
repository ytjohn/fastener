// Package proc identifies processes robustly. A pid alone can be reused, so an
// Identity carries the process start time and is only valid on the boot it was
// recorded on (callers keep the boot ID next to it).
package proc

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// ErrNoProcess means the process does not exist, is a zombie, or is not the
// process an Identity names.
var ErrNoProcess = errors.New("no such process")

// MarkerEnv tags every process of one session generation.
const MarkerEnv = "FASTENER_SESSION"

// Identity names one process instance.
type Identity struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"` // Linux: clock ticks since boot; macOS: µs since epoch
}

// Of returns the identity of a running process.
func Of(pid int) (Identity, error) {
	st, err := StartTime(pid)
	if err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, StartTime: st}, nil
}

// Self returns the identity of the calling process.
func Self() (Identity, error) { return Of(os.Getpid()) }

// Alive reports whether id still names a live (non-zombie) process on the boot bootID.
func Alive(id Identity, bootID string) bool {
	if id.PID <= 0 || bootID == "" {
		return false
	}
	cur, err := BootID()
	if err != nil || cur != bootID {
		return false
	}
	st, err := StartTime(id.PID)
	return err == nil && st == id.StartTime
}

// Signal sends sig to id after re-verifying the identity.
func Signal(id Identity, bootID string, sig syscall.Signal) error {
	if !Alive(id, bootID) {
		return ErrNoProcess
	}
	return syscall.Kill(id.PID, sig)
}

// FindMarked returns the live processes in process group pgid whose environment
// contains MarkerEnv=marker, on boot bootID. A process without the marker is
// never returned, so a reused pgid cannot redirect a signal to a stranger.
func FindMarked(pgid int, marker, bootID string) ([]Identity, error) {
	if pgid <= 0 || marker == "" {
		return nil, nil
	}
	cur, err := BootID()
	if err != nil {
		return nil, err
	}
	if cur != bootID {
		return nil, nil
	}
	pids, err := PIDs()
	if err != nil {
		return nil, err
	}
	want := MarkerEnv + "=" + marker
	var out []Identity
	for _, pid := range pids {
		g, err := PGID(pid)
		if err != nil || g != pgid {
			continue
		}
		env, err := Environ(pid)
		if err != nil || !has(env, want) {
			continue
		}
		id, err := Of(pid)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// WaitGone polls until none of ids is alive or d elapses, and returns the survivors.
func WaitGone(ids []Identity, bootID string, d time.Duration) []Identity {
	deadline := time.Now().Add(d)
	for {
		var left []Identity
		for _, id := range ids {
			if Alive(id, bootID) {
				left = append(left, id)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func has(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
