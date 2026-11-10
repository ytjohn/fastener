//go:build linux

package attach

import "golang.org/x/sys/unix"

// maxSocketPath is the longest socket path that fits sockaddr_un.sun_path
// (108 bytes on Linux) with its terminating NUL.
const maxSocketPath = 107

func peerUID(fd int) (int, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}
