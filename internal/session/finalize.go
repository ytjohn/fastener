package session

import (
	"os"
	"syscall"
	"time"
)

// Finalize lets the CLI complete status.json for a session whose monitor is
// confirmed dead. It holds an exclusive flock on the session's lock file,
// re-reads status.json, and leaves an already-exited record untouched, so a
// record the monitor wrote just before dying is never overwritten and two
// CLIs never finalize at once. It reports whether it wrote.
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
		return false, err
	}
	if st.State == StateExited {
		return false, nil
	}
	mutate(&st)
	st.SchemaVersion = SchemaVersion
	st.UpdatedAt = time.Now().UTC()
	return true, WriteJSON(d.StatusPath(), &st)
}
