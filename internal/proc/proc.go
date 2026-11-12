// Package proc identifies processes robustly. A pid alone can be reused, so an
// Identity carries the process start time and is only valid on the boot it was
// recorded on (callers keep the boot ID next to it).
package proc

import (
	"errors"
	"os"
	"strings"
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

// Signal sends sig to id after re-verifying the identity. On Linux it pins the
// process with a pidfd before verifying and signals through it, so the process
// cannot exit and have its pid reused between the check and the signal.
func Signal(id Identity, bootID string, sig syscall.Signal) error {
	h, err := Open(id, bootID)
	if err != nil {
		return err
	}
	defer h.Close()
	return h.Signal(sig)
}

// Handle refers to one verified process instance. On Linux it holds a pidfd
// opened before the identity was verified, so every signal sent through it
// reaches that instance or nobody, however long the handle is held. Where
// pidfds are unavailable (macOS, Linux before 5.3, a seccomp sandbox) it holds
// only the identity and each Signal re-verifies it before kill(2), which
// leaves a small pid-reuse window. A Handle is not safe for concurrent use.
type Handle struct {
	id     Identity
	boot   string
	fd     int // pidfd, or -1 when none
	closed bool
}

// errClosed is returned by Signal on a closed Handle.
var errClosed = errors.New("proc: handle is closed")

// Open pins and verifies id: it opens a pidfd first (where available) and
// only then checks pid, start time and boot, so a process that dies after the
// open fails the check instead of being replaced by a stranger. It returns
// ErrNoProcess when id is not alive, and another error when liveness could not
// be determined. The caller must Close the handle.
func Open(id Identity, bootID string) (*Handle, error) {
	if id.PID <= 0 || bootID == "" {
		return nil, ErrNoProcess
	}
	fd, err := openPin(id.PID)
	if err != nil {
		return nil, err
	}
	h := &Handle{id: id, boot: bootID, fd: fd}
	ok, err := Check(id, bootID)
	if err == nil && !ok {
		err = ErrNoProcess
	}
	if err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Identity returns the identity the handle was opened for.
func (h *Handle) Identity() Identity { return h.id }

// Alive is true only when Check confirms the process is still alive.
func (h *Handle) Alive() bool { return Alive(h.id, h.boot) }

// Signal sends sig to the process. ErrNoProcess means it has exited (on Linux
// with a pidfd: been reaped; a zombie still accepts and ignores the signal).
func (h *Handle) Signal(sig syscall.Signal) error {
	if h.closed {
		return errClosed
	}
	if h.fd >= 0 {
		return signalPin(h.fd, sig)
	}
	ok, err := Check(h.id, h.boot)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoProcess
	}
	if err := syscall.Kill(h.id.PID, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrNoProcess
		}
		return err
	}
	return nil
}

// Close releases the pidfd, if any. It is idempotent.
func (h *Handle) Close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.fd >= 0 {
		closePin(h.fd)
		h.fd = -1
	}
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
	return scanMarked(pgid, marker)
}

// FindMarkedAnyGroup is FindMarked without a process group, for a session whose
// status.json (and so pgid and boot ID) is unreadable. It scans the processes
// of the current boot, so it is scoped to it by construction. The marker match
// is still required for every result, and each pid gets the same start-time
// re-check; the marker is random per session generation, so a stranger cannot
// match it. The calling process is never returned.
func FindMarkedAnyGroup(marker string) ([]Identity, error) {
	if marker == "" {
		return nil, nil
	}
	return scanMarked(0, marker)
}

// scanMarked returns live processes carrying the marker; pgid 0 matches any group.
func scanMarked(pgid int, marker string) ([]Identity, error) {
	pids, err := PIDs()
	if err != nil {
		return nil, err
	}
	want := MarkerEnv + "=" + marker
	self := os.Getpid()
	var out []Identity
	for _, pid := range pids {
		if pid == self {
			continue
		}
		g, start, err := pgidStart(pid)
		if err != nil || (pgid > 0 && g != pgid) {
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

// ArgvMatch is a live process with a given string in one of its arguments.
type ArgvMatch struct {
	PID  int
	Argv []string
}

// FindArgv returns the live processes, other than the calling process and
// its ancestors (the shell or script that ran it, whose command line may
// name the same string), that have s inside one of their arguments.
// Processes whose arguments cannot be read (another user's on macOS, a
// process that exits during the scan) are skipped; only a failure to list
// processes at all is an error.
func FindArgv(s string) ([]ArgvMatch, error) {
	if s == "" {
		return nil, nil
	}
	pids, err := PIDs()
	if err != nil {
		return nil, err
	}
	skip := map[int]bool{0: true}
	for pid, n := os.Getpid(), 0; pid > 1 && n < 64 && !skip[pid]; n++ {
		skip[pid] = true
		if pid, err = PPID(pid); err != nil {
			break
		}
	}
	var out []ArgvMatch
	for _, pid := range pids {
		if skip[pid] {
			continue
		}
		argv, err := Args(pid)
		if err != nil {
			continue
		}
		for _, a := range argv {
			if strings.Contains(a, s) {
				out = append(out, ArgvMatch{PID: pid, Argv: argv})
				break
			}
		}
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
