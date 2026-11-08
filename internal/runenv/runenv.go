// Package runenv builds an agent's environment explicitly: a passthrough base
// captured from the caller, then an env file, then --env values.
package runenv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

// ReadFile parses KEY=VALUE lines; blank lines and # comments are skipped,
// an optional leading "export " is allowed, and whitespace around key and value is trimmed.
func ReadFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, n)
		}
		out = append(out, k+"="+strings.TrimSpace(v))
	}
	return out, sc.Err()
}

// Merge combines layers; a later layer's value wins, order of first appearance is kept.
func Merge(layers ...[]string) []string {
	idx := map[string]int{}
	var out []string
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
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}
