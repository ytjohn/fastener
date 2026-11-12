package finder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Located is a conversation found by its ID wherever the agent's store
// keeps it, whatever folder it was started in (`fastener adopt`).
type Located struct {
	ID string
	// Path is the conversation's file, ModTime its modification time.
	Path    string
	ModTime time.Time
	// Cwd is the folder the conversation belongs to as the store records
	// it, or "" when the store records none Fastener reads. Claude: the
	// cwd field of the first transcript line that has one; pi: the cwd of
	// the session header (the first line). Only that field is decoded.
	// Codex: always "" (`codex resume <id>` finds a rollout from any folder,
	// so the folder is the caller's choice).
	Cwd string
	// belongs reports whether the agent, started in a folder, looks for
	// the conversation where it is stored; nil means any folder.
	belongs func(cwd string) bool
}

// BelongsTo reports whether resuming the conversation with the agent started
// in cwd keeps it where the agent stores that folder's conversations: for
// Claude and pi the store directory is named after the folder (so a restore's
// finder looks there); for Codex every folder does.
func (l Located) BelongsTo(cwd string) bool {
	if l.belongs == nil {
		return true
	}
	for _, c := range cwds(cwd) {
		if l.belongs(c) {
			return true
		}
	}
	return false
}

// AnyFolder reports whether the agent resumes the conversation from any
// folder (Codex), so the folder is the caller's choice.
func (l Located) AnyFolder() bool { return l.belongs == nil }

type locator func(Query) ([]Located, error)

var locators = map[string]locator{
	"claude-transcripts": claudeLocate,
	"codex-rollouts":     codexLocate,
	"pi-sessions":        piLocate,
}

// Locate finds conversation q.SessionID in the store of the named finder,
// whatever folder it belongs to; q.Cwd may be empty (it only anchors a
// relative CLAUDE_CONFIG_DIR, CODEX_HOME or PI_CODING_AGENT_DIR). It
// returns one Located per file holding that ID, sorted by path, or an error
// wrapping ErrNotFound when there is none. The ID must be a UUID.
func Locate(name string, q Query) ([]Located, error) {
	loc, ok := locators[name]
	if !ok {
		return nil, fmt.Errorf("finder %q cannot locate a conversation by its ID", name)
	}
	if !ValidID(q.SessionID) {
		return nil, fmt.Errorf("invalid conversation ID %q: want a UUID", q.SessionID)
	}
	out, err := loc(q)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// globFiles is the regular files matching pattern, with their mtimes.
func globFiles(pattern string) []Located {
	matches, _ := filepath.Glob(pattern)
	var out []Located
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
			out = append(out, Located{Path: m, ModTime: fi.ModTime()})
		}
	}
	return out
}

func claudeLocate(q Query) ([]Located, error) {
	projects := filepath.Join(claudeHome(q), "projects")
	found := globFiles(filepath.Join(projects, "*", q.SessionID+".jsonl"))
	for i := range found {
		l := &found[i]
		l.ID = q.SessionID
		key := filepath.Base(filepath.Dir(l.Path))
		l.belongs = func(cwd string) bool { return claudeProjectKey(cwd) == key }
		// The transcript's cwd counts only if it is the folder its
		// directory is named after (a resumed conversation's later lines
		// may carry another cwd; the first one is where it started).
		if c := firstCwd(l.Path, maxCwdLines); c != "" && l.BelongsTo(c) {
			l.Cwd = c
		}
	}
	if len(found) == 0 {
		return nil, notFound("no transcript " + q.SessionID + ".jsonl in " + filepath.Join(projects, "*"))
	}
	return found, nil
}

func piLocate(q Query) ([]Located, error) {
	sessions := filepath.Dir(piSessionDir(q, "/"))
	found := globFiles(filepath.Join(sessions, "*", "*_"+q.SessionID+".jsonl"))
	for i := range found {
		l := &found[i]
		l.ID = q.SessionID
		dir := filepath.Base(filepath.Dir(l.Path))
		l.belongs = func(cwd string) bool { return filepath.Base(piSessionDir(q, cwd)) == dir }
		if c := firstCwd(l.Path, 1); c != "" && l.BelongsTo(c) {
			l.Cwd = c
		}
	}
	if len(found) == 0 {
		return nil, notFound("no session file *_" + q.SessionID + ".jsonl in " + filepath.Join(sessions, "*"))
	}
	return found, nil
}

func codexLocate(q Query) ([]Located, error) {
	root := filepath.Join(codexHome(q), "sessions")
	var out []Located
	for _, l := range globFiles(filepath.Join(root, "*", "*", "*", "rollout-*"+q.SessionID+".jsonl")) {
		if m, ok := readCodexMeta(l.Path); ok && m.Payload.ID == q.SessionID {
			l.ID = q.SessionID
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return nil, notFound("no rollout for " + q.SessionID + " in " + root)
	}
	return out, nil
}

// Bounds of the search for a transcript's cwd: the first lines of a
// Claude transcript are bookkeeping records without one, and the first
// message carries it. A longer line (an inlined image) is skipped.
const (
	maxCwdLines   = 200
	maxCwdLineLen = 4 << 20
	maxCwdScan    = 64 << 20
)

// firstCwd is the cwd field of the first of the file's first maxLines lines
// that has an absolute one, else "". Each line is decoded into a struct that
// has only that field, so nothing else of the conversation is kept.
func firstCwd(path string, maxLines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, maxCwdScan), 64<<10)
	for n := 0; n < maxLines; n++ {
		line, err := readBoundedLine(r, maxCwdLineLen)
		if bytes.Contains(line, []byte(`"cwd"`)) {
			var rec struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &rec) == nil && filepath.IsAbs(rec.Cwd) && !strings.ContainsRune(rec.Cwd, 0) {
				return filepath.Clean(rec.Cwd)
			}
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// readBoundedLine reads one line (without its newline); a line longer than
// max is consumed and returned as nil.
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	over := false
	for {
		chunk, isPrefix, err := r.ReadLine()
		if !over {
			line = append(line, chunk...)
			if len(line) > max {
				over, line = true, nil
			}
		}
		if err != nil {
			return line, err
		}
		if !isPrefix {
			return line, nil
		}
	}
}
