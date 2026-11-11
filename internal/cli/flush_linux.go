//go:build linux

package cli

import "golang.org/x/sys/unix"

// flushInput discards input the terminal has received but attach has not
// read (tcflush(fd, TCIFLUSH)).
func flushInput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
