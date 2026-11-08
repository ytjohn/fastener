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

// Check reports whether id names a live process on boot bootID.
// A non-nil error means liveness could not be determined.
func Check(id Identity, bootID string) (bool, error) {
	if id.PID <= 0 || bootID == "" {
		return false, nil
	}
	cur, err := BootID()
	if err != nil {
		return false, err
	}
	if cur != bootID {
		return false, nil
	}
	st, err := StartTime(id.PID)
	if errors.Is(err, ErrNoProcess) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st == id.StartTime, nil
}

// Alive is true only when Check confirms the process is alive. Use it before signalling.
func Alive(id Identity, bootID string) bool {
	ok, err := Check(id, bootID)
	return ok && err == nil
}

// MaybeAlive is false only when Check confirms the process is gone. Use it before declaring a session lost.
func MaybeAlive(id Identity, bootID string) bool {
	ok, err := Check(id, bootID)
	return ok || err != nil
}

// Signal sends sig to id after re-verifying the identity.
func Signal(id Identity, bootID string, sig syscall.Signal) error {
	if !Alive(id, bootID) {
		return ErrNoProcess
	}
	if err := syscall.Kill(id.PID, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrNoProcess
		}
		return err
	}
	return nil
}

// FindMarked returns the live processes in process group pgid whose environment
// contains MarkerEnv=marker, on boot bootID. A process without the marker is
// never returned, so a reused pgid cannot redirect a signal to a stranger.
// The caller must not be a member of the group it searches.
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
		g, start, err := pgidStart(pid)
		if err != nil || g != pgid {
			continue
		}
		env, err := Environ(pid)
		if err != nil || !has(env, want) {
			continue
		}
		// Re-check that the pid was not recycled while the environment was read.
		if again, err := StartTime(pid); err != nil || again != start {
			continue
		}
		out = append(out, Identity{PID: pid, StartTime: start})
	}
	return out, nil
}

// WaitGone polls until none of ids may be alive or d elapses, and returns the survivors.
func WaitGone(ids []Identity, bootID string, d time.Duration) []Identity {
	deadline := time.Now().Add(d)
	for {
		var left []Identity
		for _, id := range ids {
			if MaybeAlive(id, bootID) {
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
