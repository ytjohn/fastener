// Package finder holds the named conversation finders of interactive
// sessions (design §8.2, "Conversation ID"): Go functions that locate the
// conversation an interactive agent is writing in its own store, so that a
// restore can resume it. A finder reads only file names, modification times
// and, where it must, a few metadata fields of a file's first line; never a
// conversation's content.
//
// The exact method is an ID Fastener assigns at launch (the profile's
// [interactive] session_id): the finder then only checks that the agent has
// written that conversation. Without one, a finder falls back to a heuristic
// and reports ErrAmbiguous rather than guess between candidates.
package finder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Query is what a finder knows about one generation of a session.
type Query struct {
	// Cwd is the session's working directory (absolute). The agents name
	// their stores after their physical cwd (getcwd), so finders resolve
	// symlinks; they also try the path as given.
	Cwd string
	// StartedAt is when the agent was spawned.
	StartedAt time.Time
	// Env is the agent's environment, for the variables that move a store
	// (CLAUDE_CONFIG_DIR, CODEX_HOME, PI_CODING_AGENT_DIR) and HOME.
	Env []string
	// SessionID is the ID assigned at launch, if the profile assigns one.
	SessionID string
}

// Errors a finder returns when it has no ID.
var (
	// ErrNotFound: no conversation yet (agents write theirs only after the
	// first message), or none in the store.
	ErrNotFound = errors.New("no conversation found")
	// ErrAmbiguous: more than one conversation could be this session's.
	ErrAmbiguous = errors.New("more than one conversation could be this session's")
)

// Finder locates a session's conversation. It returns the conversation ID,
// or an error wrapping ErrNotFound or ErrAmbiguous (or another error when
// the store cannot be read).
type Finder func(Query) (string, error)

var finders = map[string]Finder{
	"claude-transcripts": claudeTranscripts,
	"codex-rollouts":     codexRollouts,
	"pi-sessions":        piSessions,
}

// Get returns the named finder.
func Get(name string) (Finder, bool) {
	f, ok := finders[name]
	return f, ok
}

// Names lists the finders, sorted.
func Names() []string {
	out := make([]string, 0, len(finders))
	for n := range finders {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func getenv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // the last one wins, as in a process environment
		}
	}
	return v
}

// home is the agent's HOME, else this process's.
func home(env []string) string {
	if h := getenv(env, "HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// cwds is the physical cwd first, then the path as given if it differs.
func cwds(cwd string) []string {
	out := []string{}
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		out = append(out, r)
	}
	if c := filepath.Clean(cwd); len(out) == 0 || out[0] != c {
		out = append(out, c)
	}
	return out
}

func notFound(what string) error { return wrapf(ErrNotFound, "%s", what) }

type wrapped struct {
	err error
	msg string
}

func (w wrapped) Error() string { return w.msg }
func (w wrapped) Unwrap() error { return w.err }

func wrapf(err error, format string, a ...any) error {
	return wrapped{err: err, msg: err.Error() + ": " + fmt.Sprintf(format, a...)}
}

// one picks the only candidate, or reports none or several.
func one(cands []string, where string) (string, error) {
	switch len(cands) {
	case 0:
		return "", notFound(where)
	case 1:
		return cands[0], nil
	}
	sort.Strings(cands)
	return "", wrapf(ErrAmbiguous, "%d in %s (%s)", len(cands), where, strings.Join(cands, ", "))
}
