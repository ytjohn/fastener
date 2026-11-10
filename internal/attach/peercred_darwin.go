//go:build darwin

package attach

import "golang.org/x/sys/unix"

// maxSocketPath is the longest socket path that fits sockaddr_un.sun_path
// (104 bytes on macOS) with its terminating NUL.
const maxSocketPath = 103

func peerUID(fd int) (int, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}
