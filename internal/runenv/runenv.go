// Package runenv builds an agent's environment explicitly: a passthrough base
// captured from the caller, then an env file, then --env values.
package runenv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DefaultPassthrough is the default env_passthrough list (spec §5).
var DefaultPassthrough = []string{
	"HOME", "USER", "LOGNAME", "PATH", "LANG", "LC_*", "SSH_AUTH_SOCK",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
}

// Base copies variables named in passthrough from environ. "X_*" matches a prefix.
func Base(passthrough, environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, p := range passthrough {
			if p == k || (strings.HasSuffix(p, "*") && strings.HasPrefix(k, strings.TrimSuffix(p, "*"))) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// ReadFile parses KEY=VALUE lines; blank lines and whole-line # comments are
// skipped, an optional leading "export " is allowed, and whitespace around key
// and value is trimmed. If a value is wrapped in one matching pair of " or ',
// that pair is stripped; quotes inside a value stay as written. A # inside a
// value is literal (only whole-line comments exist). On error it returns nil.
func ReadFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		text := sc.Text()
		if n == 1 {
			text = strings.TrimPrefix(text, "\uFEFF")
		}
		line := strings.TrimSpace(text)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, n)
		}
		if !keyRE.MatchString(k) {
			return nil, fmt.Errorf("%s:%d: invalid key %q", path, n, k)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out = append(out, k+"="+v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// Merge combines layers; a later layer's value wins, order of first appearance is kept.
func Merge(layers ...[]string) []string {
	idx := map[string]int{}
	out := []string{}
	for _, layer := range layers {
		for _, kv := range layer {
			k, _, _ := strings.Cut(kv, "=")
			if i, ok := idx[k]; ok {
				out[i] = kv
				continue
			}
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// Get returns the value of key in env, or "".
func Get(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// LookPath finds name on pathList (colon-separated). Relative PATH entries are
// ignored. A name containing '/' must be an absolute executable path.
func LookPath(name, pathList string) (string, error) {
	if strings.ContainsRune(name, '/') {
		if filepath.IsAbs(name) && isExec(name) {
			return name, nil
		}
		return "", fmt.Errorf("%q is not an absolute path to an executable", name)
	}
	for _, dir := range filepath.SplitList(pathList) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if isExec(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q not found on the session PATH (%s)", name, pathList)
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && unix.Access(p, unix.X_OK) == nil
}
