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

// Listen creates the attach socket for the session in dir and returns the
// listener and the path it is bound to (see SocketPath). The socket is
// created inside a directory only this user can enter (the session
// directory, or the checked fallback directory), then chmodded to 0600. A
// leftover socket from a monitor that died is replaced; any other file at
// the path is an error. Closing the listener removes the socket.
func Listen(dir string) (*net.UnixListener, string, error) {
	path, fallback := SocketPath(dir)
	if fallback {
		if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
			return nil, "", fmt.Errorf("attach socket: %s/%s is %d bytes, over the %d-byte socket address limit, and the fallback directory is unusable: %w",
				dir, SocketName, len(filepath.Join(dir, SocketName)), maxSocketPath, err)
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, "", fmt.Errorf("attach socket: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, "", fmt.Errorf("attach socket: remove stale %s: %w", path, err)
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, "", fmt.Errorf("attach socket: %w", err)
	}
	l.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, "", fmt.Errorf("attach socket: %w", err)
	}
	return l, path, nil
}

// peerIsOwner reports whether the process at the other end of c runs as this
// user, from the kernel's peer credentials (SO_PEERCRED on Linux,
// LOCAL_PEERCRED on macOS).
func peerIsOwner(c *net.UnixConn) (bool, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return false, err
	}
	var uid int
	var cerr error
	if err := rc.Control(func(fd uintptr) { uid, cerr = peerUID(int(fd)) }); err != nil {
		return false, err
	}
	if cerr != nil {
		return false, cerr
	}
	return uid == os.Getuid(), nil
}
