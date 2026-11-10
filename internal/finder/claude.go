package finder

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
	"unicode/utf16"
)

// Claude Code (verified against 2.1.322) keeps an interactive session's
// transcript at <config home>/projects/<project key>/<session id>.jsonl,
// where the config home is $CLAUDE_CONFIG_DIR, else ~/.claude, and the
// session ID is a UUID (--session-id <uuid> sets it). The file appears with
// the first message, not when the TUI starts.

// claudeProjectKey is Claude Code's directory name for a cwd: every
// character other than an ASCII letter or digit becomes "-"; a key longer
// than 200 characters is cut to 200 and followed by "-" and the base-36
// absolute value of the cwd's Java-style string hash (over UTF-16 code
// units). Claude names the directory after its physical cwd
// (process.cwd(), so symlinks resolved), and a worktree after the
// worktree's own path.
func claudeProjectKey(cwd string) string {
	units := utf16.Encode([]rune(cwd))
	key := make([]byte, 0, len(units))
	for _, u := range units {
		if u < 128 && (u >= 'a' && u <= 'z' || u >= 'A' && u <= 'Z' || u >= '0' && u <= '9') {
			key = append(key, byte(u))
		} else {
			key = append(key, '-')
		}
	}
	if len(key) <= 200 {
		return string(key)
	}
	var h int32
	for _, u := range units {
		h = h<<5 - h + int32(u)
	}
	abs := int64(h)
	if abs < 0 {
		abs = -abs
	}
	return string(key[:200]) + "-" + strconv.FormatInt(abs, 36)
}

func claudeHome(env []string) string {
	if d := getenv(env, "CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home(env), ".claude")
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// claudeTranscripts finds the transcript of an assigned session ID (exact),
// else the one transcript in the cwd's project directory modified since the
// session started (a heuristic: ambiguous when there are more).
func claudeTranscripts(q Query) (string, error) {
	projects := filepath.Join(claudeHome(q.Env), "projects")
	var dirs []string
	for _, c := range cwds(q.Cwd) {
		dirs = append(dirs, filepath.Join(projects, claudeProjectKey(c)))
	}
	if q.SessionID != "" {
		for _, d := range dirs {
			if fi, err := os.Stat(filepath.Join(d, q.SessionID+".jsonl")); err == nil && fi.Mode().IsRegular() {
				return q.SessionID, nil
			}
		}
		return "", notFound("no transcript " + q.SessionID + ".jsonl yet in " + dirs[0])
	}
	var cands []string
	for _, d := range dirs {
		cands = append(cands, newerFiles(d, q.StartedAt, func(name string) (string, bool) {
			id, ok := trimSuffix(name, ".jsonl")
			return id, ok && uuidRE.MatchString(id)
		})...)
	}
	return one(dedupe(cands), dirs[0])
}

// newerFiles lists, by ID, the regular files in dir that id accepts and that
// were modified at or after since.
func newerFiles(dir string, since time.Time, id func(name string) (string, bool)) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		v, ok := id(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err == nil && !fi.ModTime().Before(since) {
			out = append(out, v)
		}
	}
	return out
}

func trimSuffix(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return "", false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
