package finder

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Pi (verified against 0.84.4) keeps a session at
// <agent dir>/sessions/--<cwd without its leading "/", with "/", "\" and
// ":" as "-">--/<ISO time with ":" and "." as "-">_<session id>.jsonl,
// where the agent dir is $PI_CODING_AGENT_DIR, else ~/.pi/agent, and the
// cwd is the physical one. `--session-id <id>` opens that session if it
// exists and otherwise creates it with that ID, so the same flag both
// assigns the ID and resumes it.

func piSessionDir(env []string, cwd string) string {
	agent := getenv(env, "PI_CODING_AGENT_DIR")
	if agent == "" {
		agent = filepath.Join(home(env), ".pi", "agent")
	} else if strings.HasPrefix(agent, "~/") {
		agent = filepath.Join(home(env), agent[2:])
	}
	safe := strings.TrimPrefix(cwd, "/")
	safe = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safe)
	return filepath.Join(agent, "sessions", "--"+safe+"--")
}

// piFileTime parses the time at the start of a session file name.
func piFileTime(stamp string) (time.Time, bool) {
	// 2026-11-10T16-35-25-813Z
	if len(stamp) != len("2006-01-02T15-04-05-000Z") {
		return time.Time{}, false
	}
	// Back to RFC 3339: the "-" after the date's "T" were ":" and ".".
	b := []byte(stamp)
	b[13], b[16], b[19] = ':', ':', '.'
	t, err := time.Parse("2006-01-02T15:04:05.000Z", string(b))
	return t, err == nil
}

// piSessions finds the session file of an assigned ID (exact), else the one
// session created (by the time in its name) since the session started.
func piSessions(q Query) (string, error) {
	var dirs []string
	for _, c := range cwds(q.Cwd) {
		dirs = append(dirs, piSessionDir(q.Env, c))
	}
	var cands []string
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			base, ok := trimSuffix(e.Name(), ".jsonl")
			stamp, id, cut := strings.Cut(base, "_")
			if !ok || !cut || id == "" || !e.Type().IsRegular() {
				continue
			}
			if q.SessionID != "" {
				if id == q.SessionID {
					return id, nil
				}
				continue
			}
			// Pi's file time has millisecond precision; allow for it.
			if t, ok := piFileTime(stamp); ok && !t.Before(q.StartedAt.Add(-time.Second)) {
				cands = append(cands, id)
			}
		}
	}
	if q.SessionID != "" {
		return "", notFound("no session file *_" + q.SessionID + ".jsonl yet in " + dirs[0])
	}
	return one(dedupe(cands), dirs[0])
}
