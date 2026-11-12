//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

const sZomb = 5 // SZOMB in <sys/proc.h>

func kinfo(pid int) (*unix.KinfoProc, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EIO) {
			return nil, ErrNoProcess
		}
		return nil, err
	}
	if int(k.Proc.P_pid) != pid {
		return nil, ErrNoProcess
	}
	return k, nil
}

func StartTime(pid int) (uint64, error) {
	k, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	if k.Proc.P_stat == sZomb {
		return 0, ErrNoProcess
	}
	tv := k.Proc.P_starttime
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), nil
}

// pgidStart reads pgid and start time from one kinfo call (zombies are ErrNoProcess).
func pgidStart(pid int) (int, uint64, error) {
	k, err := kinfo(pid)
	if err != nil {
		return 0, 0, err
	}
	if k.Proc.P_stat == sZomb {
		return 0, 0, ErrNoProcess
	}
	tv := k.Proc.P_starttime
	return int(k.Eproc.Pgid), uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), nil
}

// IsZombie reports whether pid has exited but not been reaped yet (p_stat is
// SZOMB). A pid with no process at all is ErrNoProcess.
func IsZombie(pid int) (bool, error) {
	k, err := kinfo(pid)
	if err != nil {
		return false, err
	}
	return k.Proc.P_stat == sZomb, nil
}

func PGID(pid int) (int, error) {
	k, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(k.Eproc.Pgid), nil
}

// BootID uses kern.bootsessionuuid; kern.boottime shifts with clock adjustments.
func BootID() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }

func PIDs() ([]int, error) {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(ks))
	for _, k := range ks {
		out = append(out, int(k.Proc.P_pid))
	}
	return out, nil
}

// Environ parses KERN_PROCARGS2: int32 argc, exec path, NUL padding,
// argv[argc] (NUL-terminated), then environment strings until an empty one.
func Environ(pid int) ([]string, error) {
	_, env, err := procArgs(pid)
	return env, err
}

// Args returns the process's argument list (KERN_PROCARGS2).
func Args(pid int) ([]string, error) {
	argv, _, err := procArgs(pid)
	return argv, err
}

// PPID returns the parent process ID.
func PPID(pid int) (int, error) {
	k, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(k.Eproc.Ppid), nil
}

func procArgs(pid int) (argv, env []string, err error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, err
	}
	if len(b) < 4 {
		return nil, nil, ErrNoProcess
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	i := bytes.IndexByte(b, 0) // end of exec path
	if i < 0 {
		return nil, nil, nil
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	for n := 0; n < argc; n++ {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return argv, nil, nil
		}
		argv = append(argv, string(b[:i]))
		b = b[i+1:]
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, 0)
		if i <= 0 {
			break
		}
		env = append(env, string(b[:i]))
		b = b[i+1:]
	}
	return argv, env, nil
}
