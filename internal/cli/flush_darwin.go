//go:build darwin

package cli

import "golang.org/x/sys/unix"

// flushInput discards input the terminal has received but attach has not
// read (tcflush(fd, TCIFLUSH)).
func flushInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread) }

// fread is FREAD from <sys/fcntl.h>, which x/sys/unix does not export.
const fread = 0x1
