// Package claudetrust records Claude Code's workspace trust for a folder by
// setting projects[<key>].hasTrustDialogAccepted in Claude's global config,
// the way Claude Code 2.1.319 itself saves it (design §8.3). Only
// `fastener authorize claude-folder-trust` calls it.
package claudetrust

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// Key is Claude Code's trust key for a resolved folder: the root of the git
// repository the folder is in (`git rev-parse --show-toplevel`, symlinks
// resolved), else the folder itself; with forward slashes. inRepo reports
// whether the key is a repository root. A key that is the home directory or
// the root is refused. When git is missing or the folder is not in a
// repository (or git fails for any other reason), the folder is the key.
func Key(folder string) (key string, inRepo bool, err error) {
	key = folder
	if top, ok := gitToplevel(folder); ok {
		key, inRepo = top, true
	}
	if err := refuse(key, "trust key"); err != nil {
		if inRepo {
			err = fmt.Errorf("%w (%s is inside that git repository, and Claude keys trust by the repository root)", err, folder)
		}
		return "", false, err
	}
	return filepath.ToSlash(key), inRepo, nil
}

func gitToplevel(folder string) (string, bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "-C", folder, "rev-parse", "--show-toplevel")
	// Variables that would point git somewhere other than the folder itself.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	top := strings.TrimRight(string(out), "\r\n")
	if top == "" {
		return "", false
	}
	real, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", false
	}
	return filepath.Clean(real), true
}
