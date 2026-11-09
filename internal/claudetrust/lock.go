package claudetrust

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Claude Code saves its config under a proper-lockfile lock: the lock is the
// directory "<real path of the file>.lock", taken by mkdir (atomic: success
// means you hold it) and released by rmdir. The holder refreshes the lock's
// mtime every stale/2, and a lock whose mtime is older than stale (10 s by
// default) is considered abandoned. Fastener takes the same lock.

const (
	// DefaultLockTimeout is how long to wait for a lock held by someone else.
	DefaultLockTimeout = 10 * time.Second
	// DefaultLockStale is proper-lockfile's default stale time.
	DefaultLockStale = 10 * time.Second
)

// ErrLockTimeout means the lock was still held (and fresh) when the wait ran out.
var ErrLockTimeout = errors.New("timed out waiting for the lock")

// heldLock is a lock directory this process created.
type heldLock struct {
	path string
	stop chan struct{}
	wg   sync.WaitGroup
}

// acquireLock creates the lock directory lockPath, retrying with backoff
// while a fresh lock exists, for up to timeout. A lock whose mtime is older
// than stale is removed (rmdir) and the mkdir retried; a fresh lock is never
// removed. While held, the lock's mtime is refreshed every stale/2.
func acquireLock(lockPath string, timeout, stale time.Duration) (*heldLock, error) {
	deadline := time.Now().Add(timeout)
	backoff := 20 * time.Millisecond
	for {
		err := os.Mkdir(lockPath, 0o777)
		if err == nil {
			l := &heldLock{path: lockPath, stop: make(chan struct{})}
			l.refresh(stale / 2)
			return l, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("creating lock %s: %w", lockPath, err)
		}
		fi, serr := os.Stat(lockPath)
		switch {
		case errors.Is(serr, fs.ErrNotExist):
			continue // released between mkdir and stat: retry at once
		case serr != nil:
			return nil, fmt.Errorf("checking lock %s: %w", lockPath, serr)
		case time.Since(fi.ModTime()) > stale:
			// Abandoned (its holder stopped refreshing it). rmdir fails on a
			// non-empty directory, so this only removes a lock directory.
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
// not mistaken for an abandoned lock.
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
				now := time.Now()
				_ = os.Chtimes(l.path, now, now)
			}
		}
	}()
}

// release stops the refresher and removes the lock directory.
func (l *heldLock) release() error {
	close(l.stop)
	l.wg.Wait()
	return os.Remove(l.path)
}
