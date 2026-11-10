package attach

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// SocketName is the attach socket's name in the session directory.
const SocketName = "attach.sock"

// FallbackRoot is where a socket goes when the session directory's path is
// too long for a Unix socket address: /tmp/fastener-<uid>/. It is a variable
// so tests can move it.
var FallbackRoot = "/tmp"

// SocketPath returns where the monitor of the session in dir listens:
// <dir>/attach.sock when that path fits in a Unix socket address
// (maxSocketPath bytes), else /tmp/fastener-<uid>/<hash>.sock, where hash is
// the first 16 hex digits of the SHA-256 of dir. fallback reports which.
func SocketPath(dir string) (path string, fallback bool) {
	p := filepath.Join(dir, SocketName)
	if len(p) <= maxSocketPath {
		return p, false
	}
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(fallbackDir(), hex.EncodeToString(sum[:8])+".sock"), true
}

func fallbackDir() string {
	return filepath.Join(FallbackRoot, "fastener-"+strconv.Itoa(os.Getuid()))
}

// ensurePrivateDir makes dir (mode 0700) or checks that an existing one is
// a real directory (not a symlink), owned by this user, with no group or
// other permission bits. Anything else is an error: the fallback lives in a
// shared directory, where another user could have planted it.
func ensurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("%s is not owned by uid %d", dir, os.Getuid())
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s has mode %v; want no group or other access", dir, fi.Mode().Perm())
	}
	return nil
}

// Listener is the attach socket's listener. Closing it removes the socket
// file only if the path still names the socket it bound, so a monitor that
// outlives its successor's start can never delete the successor's socket.
type Listener struct {
	*net.UnixListener
	path  string
	bound os.FileInfo
}

// Close removes the socket if it is still ours, then stops listening. The
// check comes first: while our fd is open the inode cannot be freed, so a
// successor's socket can never be taken for ours by a reused inode number.
func (l *Listener) Close() error {
	if fi, err := os.Lstat(l.path); err == nil && os.SameFile(fi, l.bound) {
		_ = os.Remove(l.path)
	}
	return l.UnixListener.Close()
}

// Listen creates the attach socket for the session in dir and returns the
// listener and the path it is bound to (see SocketPath). The socket is
// created inside a directory only this user can enter (the session
// directory, or the checked fallback directory), then chmodded to 0600.
//
// A socket already at the path is replaced only if it is stale: connecting
// to it must fail with ECONNREFUSED (a monitor that was killed). If the
// connect succeeds, another monitor is serving there and Listen fails; any
// other error, or any other kind of file at the path, is an error too.
func Listen(dir string) (*Listener, string, error) {
	path, fallback := SocketPath(dir)
	if fallback {
		if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
			return nil, "", fmt.Errorf("attach socket: %s/%s is %d bytes, over the %d-byte socket address limit, and the fallback directory is unusable: %w",
				dir, SocketName, len(filepath.Join(dir, SocketName)), maxSocketPath, err)
		}
	}
	if err := removeStale(path); err != nil {
		return nil, "", fmt.Errorf("attach socket: %w", err)
	}
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, "", fmt.Errorf("attach socket: %w", err)
	}
	ul.SetUnlinkOnClose(false) // Listener.Close unlinks, after checking the inode
	fi, err := os.Lstat(path)
	if err == nil {
		err = os.Chmod(path, 0o600)
	}
	if err != nil {
		ul.Close()
		return nil, "", fmt.Errorf("attach socket: %w", err)
	}
	return &Listener{UnixListener: ul, path: path, bound: fi}, path, nil
}

// removeStale removes a dead socket at path (see Listen).
func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("%s is in use: another monitor is listening on it", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%s exists and its liveness is unknown: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale %s: %w", path, err)
	}
	return nil
}

// peerUIDOf returns the uid of the process at the other end of c, from the
// kernel's peer credentials (SO_PEERCRED on Linux, LOCAL_PEERCRED on macOS).
func peerUIDOf(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var cerr error
	if err := rc.Control(func(fd uintptr) { uid, cerr = peerUID(int(fd)) }); err != nil {
		return -1, err
	}
	return uid, cerr
}
