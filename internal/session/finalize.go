package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

// ErrMonitorAlive is returned by Finalize when a recorded monitor is still alive.
var ErrMonitorAlive = errors.New("monitor is still alive")

// Finalize lets the CLI complete status.json for a session whose monitor is
// confirmed dead. It holds an exclusive flock on the session's lock file,
// re-reads status.json, refuses (ErrMonitorAlive) if the monitor recorded in
// status.json or the current generation's launcher record is alive, and leaves an already-exited record untouched, so a
// record the monitor wrote just before dying is never overwritten and two
// CLIs never finalize at once. The residual case, where nothing records the
// monitor at all, is narrowed by the monitor writing its identity first (Task 9). It reports whether it wrote.
func Finalize(d Dir, mutate func(*Status)) (bool, error) {
	f, err := os.OpenFile(d.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var st Status
	if err := ReadJSON(d.StatusPath(), &st); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		st = Status{State: StateStarting} // half-created session
	}
	if st.State == StateExited {
		return false, nil
	}
	if st.Monitor != nil {
		if err := monitorLive(*st.Monitor, st.BootID); err != nil {
			return false, err
		}
	}
	gen := st.Generation
	if gen <= 0 {
		gen = 1
	}
	if l := readLauncher(d, gen); l != nil && l.Monitor != nil {
		if err := monitorLive(*l.Monitor, l.BootID); err != nil {
			return false, err
		}
	}
	read := st.SchemaVersion
	mutate(&st)
	st.SchemaVersion = max(read, SchemaVersion)
	st.UpdatedAt = time.Now().UTC()
	return true, WriteJSON(d.StatusPath(), &st)
}

// monitorLive returns ErrMonitorAlive if the monitor is alive or its liveness
// cannot be determined (the latter wraps the cause).
func monitorLive(id proc.Identity, boot string) error {
	ok, err := proc.Check(id, boot)
	if err != nil {
		return fmt.Errorf("%w (liveness unknown: %v)", ErrMonitorAlive, err)
	}
	if ok {
		return ErrMonitorAlive
	}
	return nil
}
