//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

const sZomb = 5 // SZOMB in <sys/proc.h>

func kinfo(pid int) (*unix.KinfoProc, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, ErrNoProcess
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
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, ErrNoProcess
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	i := bytes.IndexByte(b, 0) // end of exec path
	if i < 0 {
		return nil, nil
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	for n := 0; n < argc; n++ {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, nil
		}
		b = b[i+1:]
	}
	var env []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, 0)
		if i <= 0 {
			break
		}
		env = append(env, string(b[:i]))
		b = b[i+1:]
	}
	return env, nil
}
