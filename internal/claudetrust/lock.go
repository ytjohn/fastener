package claudetrust

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Claude Code saves its config under a proper-lockfile lock with an explicit
// lockfilePath of "<config path>.lock", where the config path is the one
// Claude computes, NOT resolved through symlinks. The lock is a directory,
// taken by mkdir (atomic: success means you hold it) and released by rmdir.
// The holder refreshes the lock's mtime every stale/2, and a lock whose mtime
// is older than stale (10 s by default) is considered abandoned. Before
// committing, proper-lockfile treats a lock whose mtime is not the one it
// last set as compromised. Fastener takes the same lock the same way.

const (
	// DefaultLockTimeout is how long to wait for a lock held by someone else.
	DefaultLockTimeout = 10 * time.Second
	// DefaultLockStale is proper-lockfile's default stale time.
	DefaultLockStale = 10 * time.Second
)

// ErrLockTimeout means the lock was still held (and fresh) when the wait ran out.
var ErrLockTimeout = errors.New("timed out waiting for the lock")

// ErrLockCompromised means the lock directory vanished, was replaced, or its
// mtime is not the one this process last set, so another process may think
// it holds the lock.
var ErrLockCompromised = errors.New("lock compromised")

// heldLock is a lock directory this process created.
type heldLock struct {
	path string
	stop chan struct{}
	wg   sync.WaitGroup

	mu         sync.Mutex
	lastMtime  time.Time // the lock's mtime as last set (and read back) by us
	refreshErr error     // the first refresh failure, if any
}

// acquireLock creates the lock directory lockPath, retrying with backoff
// while a fresh lock exists, for up to timeout. A lock directory whose mtime
// is older than stale is removed (rmdir) and the mkdir retried; a fresh lock
// is never removed, and a lock path that is not a directory is an error
// (it is never unlinked). While held, the lock's mtime is refreshed every
// stale/2.
func acquireLock(lockPath string, timeout, stale time.Duration) (*heldLock, error) {
	deadline := time.Now().Add(timeout)
	backoff := 20 * time.Millisecond
	for {
		err := os.Mkdir(lockPath, 0o777)
		if err == nil {
			fi, serr := os.Stat(lockPath)
			if serr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("checking new lock %s: %w", lockPath, serr)
			}
			l := &heldLock{path: lockPath, stop: make(chan struct{}), lastMtime: fi.ModTime()}
			l.refresh(stale / 2)
			return l, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("creating lock %s: %w", lockPath, err)
		}
		fi, serr := os.Lstat(lockPath)
		switch {
		case errors.Is(serr, fs.ErrNotExist):
			continue // released between mkdir and stat: retry at once
		case serr != nil:
			return nil, fmt.Errorf("checking lock %s: %w", lockPath, serr)
		case !fi.IsDir():
			return nil, fmt.Errorf("lock %s exists but is not a directory; remove it by hand if no Claude Code is running", lockPath)
		case time.Since(fi.ModTime()) > stale:
			// Abandoned (its holder stopped refreshing it). os.Remove on a
			// directory is rmdir, which fails if it is not empty.
			if rerr := os.Remove(lockPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				return nil, fmt.Errorf("removing stale lock %s: %w", lockPath, rerr)
			}
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w %s after %s (a running Claude Code holds it; try again)", ErrLockTimeout, lockPath, timeout)
		}
		sleep := backoff
		if rem := time.Until(deadline); sleep > rem {
			sleep = rem
		}
		time.Sleep(sleep)
		if backoff *= 2; backoff > 500*time.Millisecond {
			backoff = 500 * time.Millisecond
		}
	}
}

// refresh touches the lock every interval until release, so a long hold is
// not mistaken for an abandoned lock. Each new mtime is read back and kept
// for check; a failure is recorded.
func (l *heldLock) refresh(interval time.Duration) {
	if interval <= 0 {
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				l.touch()
			}
		}
	}()
}

func (l *heldLock) touch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refreshErr != nil {
		return
	}
	// Refuse to touch a lock that is no longer ours, as proper-lockfile does.
	if err := l.checkLocked(); err != nil {
		l.refreshErr = err
		return
	}
	now := time.Now()
	if err := os.Chtimes(l.path, now, now); err != nil {
		l.refreshErr = fmt.Errorf("refreshing lock %s: %w", l.path, err)
		return
	}
	fi, err := os.Stat(l.path)
	if err != nil {
		l.refreshErr = fmt.Errorf("refreshing lock %s: %w", l.path, err)
		return
	}
	l.lastMtime = fi.ModTime()
}

// check reports whether the lock is still ours: no refresh failed, and the
// lock directory exists with the mtime we last set.
func (l *heldLock) check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refreshErr != nil {
		return fmt.Errorf("%w: %v", ErrLockCompromised, l.refreshErr)
	}
	return l.checkLocked()
}

func (l *heldLock) checkLocked() error {
	fi, err := os.Lstat(l.path)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrLockCompromised, l.path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is no longer a directory", ErrLockCompromised, l.path)
	}
	if !fi.ModTime().Equal(l.lastMtime) {
		return fmt.Errorf("%w: %s has mtime %s, not the %s this process set", ErrLockCompromised, l.path,
			fi.ModTime().Format(time.RFC3339Nano), l.lastMtime.Format(time.RFC3339Nano))
	}
	return nil
}

// release stops the refresher and removes the lock directory, unless it is
// no longer ours (then it is left alone and an error returned).
func (l *heldLock) release() error {
	close(l.stop)
	l.wg.Wait()
	if err := l.check(); err != nil {
		return err
	}
	return os.Remove(l.path)
}

// heldLocks is several locks taken in order and released in reverse.
type heldLocks []*heldLock

// SecondLockTimeout caps the wait for each lock after the first. While we
// hold Claude's own lock, a running Claude is blocked on it with its own
// retry budget; waiting the full timeout for a second lock could starve it
// past that budget, so we give up on the second lock sooner and let go.
const SecondLockTimeout = 2 * time.Second

// acquireLocks takes paths in order: the first with timeout, the rest with
// at most SecondLockTimeout each. On failure the ones taken are released.
func acquireLocks(paths []string, timeout, stale time.Duration) (heldLocks, error) {
	var ls heldLocks
	for i, p := range paths {
		t := timeout
		if i > 0 && t > SecondLockTimeout {
			t = SecondLockTimeout
		}
		l, err := acquireLock(p, t, stale)
		if err != nil {
			_ = ls.release()
			return nil, err
		}
		ls = append(ls, l)
	}
	return ls, nil
}

func (ls heldLocks) check() error {
	for _, l := range ls {
		if err := l.check(); err != nil {
			return err
		}
	}
	return nil
}

func (ls heldLocks) release() error {
	var first error
	for i := len(ls) - 1; i >= 0; i-- {
		if err := ls[i].release(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
