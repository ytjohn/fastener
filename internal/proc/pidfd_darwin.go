//go:build darwin

package proc

import (
	"errors"
	"syscall"
)

// macOS has no pidfd: a Handle holds only the identity and re-verifies it
// before each kill(2).
func openPin(int) (int, error) { return -1, nil }

func signalPin(int, syscall.Signal) error { return errors.New("proc: no pidfd on darwin") }

func closePin(int) {}
