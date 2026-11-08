package session

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames old to new, failing with EEXIST (fs.ErrExist) if
// new exists. Filesystems without RENAME_EXCL fall back to stat-then-rename.
func renameNoReplace(old, new string) error {
	err := unix.RenameatxNp(unix.AT_FDCWD, old, unix.AT_FDCWD, new, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return renameCheckFirst(old, new)
	}
	return err
}
