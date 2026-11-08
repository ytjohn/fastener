package session

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames old to new, failing with EEXIST (fs.ErrExist) if
// new exists. Kernels or filesystems without RENAME_NOREPLACE fall back to
// stat-then-rename.
func renameNoReplace(old, new string) error {
	err := unix.Renameat2(unix.AT_FDCWD, old, unix.AT_FDCWD, new, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return renameCheckFirst(old, new)
	}
	return err
}
