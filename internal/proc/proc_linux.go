//go:build linux

package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func readStat(pid int) (state byte, pgid int, start uint64, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			err = ErrNoProcess
		}
		return 0, 0, 0, err
	}
	return parseStat(pid, string(b))
}

// parseStat parses the contents of /proc/<pid>/stat. comm (field 2) may contain
// spaces and parentheses, so fields are counted from after the last ')'.
func parseStat(pid int, s string) (state byte, pgid int, start uint64, err error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, 0, fmt.Errorf("proc: malformed stat for pid %d", pid)
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state); field N is f[N-3]
	if len(f) < 20 {
		return 0, 0, 0, fmt.Errorf("proc: short stat for pid %d", pid)
	}
	if pgid, err = strconv.Atoi(f[2]); err != nil { // field 5
		return 0, 0, 0, err
	}
	if start, err = strconv.ParseUint(f[19], 10, 64); err != nil { // field 22
		return 0, 0, 0, err
	}
	return f[0][0], pgid, start, nil
}

func deadState(s byte) bool { return s == 'Z' || s == 'X' || s == 'x' }

// pgidStart reads pgid and start time in one /proc read (zombies are ErrNoProcess).
func pgidStart(pid int) (int, uint64, error) {
	state, pgid, start, err := readStat(pid)
	if err != nil {
		return 0, 0, err
	}
	if deadState(state) {
		return 0, 0, ErrNoProcess
	}
	return pgid, start, nil
}

// StartTime returns the start time of a live process (ErrNoProcess for zombies).
func StartTime(pid int) (uint64, error) {
	state, _, start, err := readStat(pid)
	if err != nil {
		return 0, err
	}
	if deadState(state) {
		return 0, ErrNoProcess
	}
	return start, nil
}

// IsZombie reports whether pid has exited but not been reaped yet. A pid with
// no process at all is ErrNoProcess.
func IsZombie(pid int) (bool, error) {
	state, _, _, err := readStat(pid)
	if err != nil {
		return false, err
	}
	return deadState(state), nil
}

func PGID(pid int) (int, error) {
	_, pgid, _, err := readStat(pid)
	return pgid, err
}

func BootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Environ returns the process's initial environment (readable for own processes).
func Environ(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\x00"), nil
}

// Args returns the process's argument list (/proc/<pid>/cmdline; empty for
// a kernel thread or a zombie).
func Args(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			err = ErrNoProcess
		}
		return nil, err
	}
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\x00"), nil
}

// PPID returns the parent process ID (field 4 of /proc/<pid>/stat).
func PPID(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			err = ErrNoProcess
		}
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("proc: malformed stat for pid %d", pid)
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state), f[1] field 4 (ppid)
	if len(f) < 2 {
		return 0, fmt.Errorf("proc: short stat for pid %d", pid)
	}
	return strconv.Atoi(f[1])
}

func PIDs() ([]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, pid)
		}
	}
	return out, nil
}
