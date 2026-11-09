// Package claudetrust records Claude Code's workspace trust for a folder by
// setting projects[<key>].hasTrustDialogAccepted in Claude's global config,
// the way Claude Code 2.1.319 itself saves it (design §8.3). Only
// `fastener authorize claude-folder-trust` calls it.
package claudetrust

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ResolveFolder returns dir as an absolute, symlink-free path. It must exist
// and be a directory, and it must not be the home directory (Claude never
// persists trust for it) or the filesystem root.
func ResolveFolder(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("no folder given")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("folder %s: %w", dir, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("folder %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("folder %s is not a directory", dir)
	}
	if err := refuse(real, "folder"); err != nil {
		return "", err
	}
	return real, nil
}

// refuse rejects the home directory and the filesystem root.
func refuse(p, what string) error {
	if filepath.Dir(p) == p {
		return fmt.Errorf("refusing to trust %s %s: it is the filesystem root", what, p)
	}
	if isHome(p) {
		return fmt.Errorf("refusing to trust %s %s: it is the home directory, whose trust Claude Code never saves", what, p)
	}
	return nil
}

func isHome(p string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if filepath.Clean(home) == p {
		return true
	}
	real, err := filepath.EvalSymlinks(home)
	return err == nil && real == p
}

// Key is Claude Code's trust key for a resolved folder, computed as Claude
// Code 2.1.319 does (it runs no git command for this):
//
//   - Walk up from the folder to the nearest directory holding a ".git"
//     entry that is a directory or a regular file (a symlink counts if it
//     leads to one). That directory is the repository root.
//   - If ".git" is a file that names a linked worktree ("gitdir: <dir>",
//     where <dir>/commondir points at the main repository's git directory,
//     <dir> sits in that directory's "worktrees", and <dir>/gitdir points
//     back at this ".git"), the key is the main repository's root (the
//     parent of its ".git"; for a common dir not named ".git", the common
//     dir itself). Any check failing keeps the worktree's own root. A
//     submodule's ".git" file names a directory without commondir, so a
//     submodule keys to its own root.
//   - Outside a repository, the key is the folder itself.
//
// Forward slashes. inRepo reports whether the key came from a repository. A
// key that is the home directory or the filesystem root is refused.
func Key(folder string) (key string, inRepo bool, err error) {
	key = folder
	if root, ok := findGitRoot(folder); ok {
		key, inRepo = canonicalGitRoot(root), true
	}
	if err := refuse(key, "trust key"); err != nil {
		if inRepo {
			err = fmt.Errorf("%w (%s is inside that git repository, and Claude keys trust by the repository root)", err, folder)
		}
		return "", false, err
	}
	if !utf8.ValidString(key) || strings.ContainsRune(key, utf8.RuneError) {
		return "", false, fmt.Errorf("trust key %q is not valid UTF-8", key)
	}
	return filepath.ToSlash(key), inRepo, nil
}

// findGitRoot walks up from dir (inclusive) to the nearest directory with a
// usable ".git" entry.
func findGitRoot(dir string) (string, bool) {
	r := filepath.Clean(dir)
	for {
		if gitEntryOK(filepath.Join(r, ".git")) {
			return r, true
		}
		parent := filepath.Dir(r)
		if parent == r {
			return "", false
		}
		r = parent
	}
}

// gitEntryOK: a directory or regular file, or a symlink that leads to one.
func gitEntryOK(p string) bool {
	fi, err := os.Stat(p) // follows a symlink
	return err == nil && (fi.IsDir() || fi.Mode().IsRegular())
}

// regularFile: exists and is a regular file, not a symlink.
func regularFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// readTrimmed reads a small administrative file, trimmed like JS trim().
func readTrimmed(p string) (string, bool) {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 64<<10 {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func resolveFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// canonicalGitRoot maps a linked worktree's root to its main repository's
// root; anything else (a normal repository, a submodule, a malformed or
// tampered worktree) keeps root.
func canonicalGitRoot(root string) string {
	content, ok := readTrimmed(filepath.Join(root, ".git")) // fails for a .git directory
	if !ok || !strings.HasPrefix(content, "gitdir:") {
		return root
	}
	gitdirRef := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
	if gitdirRef == "" {
		return root
	}
	adm := resolveFrom(root, gitdirRef) // the worktree's administrative dir
	if !regularFile(filepath.Join(adm, "commondir")) {
		return root // a submodule, or not a linked worktree
	}
	commonRef, ok := readTrimmed(filepath.Join(adm, "commondir"))
	if !ok || commonRef == "" {
		return root
	}
	common := resolveFrom(adm, commonRef)
	if filepath.Dir(adm) != filepath.Join(common, "worktrees") {
		return root
	}
	if !regularFile(filepath.Join(adm, "gitdir")) {
		return root
	}
	back, ok := readTrimmed(filepath.Join(adm, "gitdir"))
	if !ok || back == "" {
		return root
	}
	backReal, err1 := filepath.EvalSymlinks(resolveFrom(adm, back))
	rootReal, err2 := filepath.EvalSymlinks(root)
	if err1 != nil || err2 != nil || backReal != filepath.Join(rootReal, ".git") {
		return root
	}
	if filepath.Base(common) != ".git" {
		if gitEntryOK(filepath.Join(common, ".git")) {
			return root
		}
		return common
	}
	return filepath.Dir(common)
}
