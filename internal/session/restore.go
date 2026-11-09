package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// OutputLogGen is a previous generation's output.log after restore rotated it.
func (d Dir) OutputLogGen(gen int) string { return d.join("output." + strconv.Itoa(gen) + ".log") }

// StderrLogGen is a previous generation's stderr.log after restore rotated it.
func (d Dir) StderrLogGen(gen int) string { return d.join("stderr." + strconv.Itoa(gen) + ".log") }

// RestoreLockPath is the flock target that serialises `fastener restore` of
// one session. It is separate from LockPath, which Finalize takes and which
// the restore path itself needs (a launch failure, a restore-failed record).
func (d Dir) RestoreLockPath() string { return d.join("restore.lock") }

// ErrRestoreBusy is returned by LockRestore when another restore of the
// session held the lock for the whole wait.
var ErrRestoreBusy = errors.New("another restore of this session is in progress")

// LockRestore takes the session's restore lock, polling for up to wait. The
// returned func releases it. The lock is an flock, so it is released by the
// kernel if the holder dies.
func LockRestore(d Dir, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(d.RestoreLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(string(d)))
		}
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrRestoreBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RotateLogs moves output.log and stderr.log to output.<gen>.log and
// stderr.<gen>.log, gen being the generation that wrote them. A missing log
// is skipped. If a rotated log already exists (it should not), the current
// one is appended to it, so nothing is overwritten.
func RotateLogs(d Dir, gen int) error {
	for _, p := range [][2]string{{d.OutputLog(), d.OutputLogGen(gen)}, {d.StderrLog(), d.StderrLogGen(gen)}} {
		if err := rotate(p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

func rotate(cur, old string) error {
	if _, err := os.Lstat(cur); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := os.Lstat(old); errors.Is(err, fs.ErrNotExist) {
		return os.Rename(cur, old)
	}
	src, err := os.Open(cur)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(old, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Remove(cur)
}

// MarkRestoreFailed records that restore could not relaunch the session:
// state restore-failed with reason (a code) and msg. Like Finalize it holds
// the session lock and refuses (ErrMonitorAlive) while a recorded monitor may
// be alive; unlike Finalize it also relabels an exited record, which is what a
// relaunched generation that ended at once (an untrusted folder) leaves. The
// exit facts of such a record are kept; the stale remote_url and
// remote_sessions are cleared.
func MarkRestoreFailed(d Dir, reason, msg string) error {
	_, err := finalize(d, true, func(s *Status) {
		now := time.Now().UTC()
		s.State, s.RestoreReason, s.Error = StateRestoreFailed, reason, msg
		// The bridge URL and sessions describe a bridge that is not running
		// and was not relaunched; do not leave them looking live.
		s.RemoteURL, s.RemoteSessions = "", nil
		if s.EndedAt == nil {
			s.EndedAt = &now
		}
	})
	return err
}
