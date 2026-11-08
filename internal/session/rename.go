package session

import (
	"io/fs"
	"os"
)

// renameCheckFirst is the fallback for renameNoReplace: it has a small race
// between the Lstat and the rename.
func renameCheckFirst(old, new string) error {
	if _, err := os.Lstat(new); err == nil {
		return &os.LinkError{Op: "rename", Old: old, New: new, Err: fs.ErrExist}
	}
	return os.Rename(old, new)
}
