package finder

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex (verified against codex-cli 0.167.0) keeps a session at
// <codex home>/sessions/YYYY/MM/DD/rollout-<local time>-<id>.jsonl, where
// the codex home is $CODEX_HOME, else ~/.codex, and the date is local. The
// file appears with the first message. Its first line is a session_meta
// record whose payload has the id, the cwd, the start time (UTC) and the
// source ("cli" for the TUI, "exec" for `codex exec`). Codex has no option
// to choose the ID, so this finder is a heuristic: the one interactive
// session for this cwd that started within CodexWindow after the Fastener
// session did. Its answer is a candidate, never the session's certain
// conversation.

func codexHome(q Query) string {
	if d := envDir(q, "CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(q), ".codex")
}

// maxMetaLine bounds how much of a rollout's first line is read: the meta
// record carries the base instructions (tens of KiB), never conversation
// content. A longer first line is skipped.
const maxMetaLine = 1 << 20

// CodexWindow bounds how long after the Fastener session's spawn a Codex
// rollout may start and still be taken for it. It narrows the case of a
// Codex the user starts by hand in the same folder later (the session's own
// conversation starts at its first message, so it cannot rule that out),
// but cannot tell apart two Codex sessions started in the window.
const CodexWindow = 10 * time.Minute

// codexLocation is the time zone of the sessions/YYYY/MM/DD directories
// (Codex uses local time); a variable for tests.
var codexLocation = time.Local

// codexDayDirs lists the day directories from the day before from to the
// day after to, by calendar date in loc (a day is not always 24 hours).
func codexDayDirs(root string, from, to time.Time, loc *time.Location) []string {
	f, t := from.In(loc), to.In(loc)
	day := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -1)
	last := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	var dirs []string
	for ; !day.After(last); day = day.AddDate(0, 0, 1) {
		dirs = append(dirs, filepath.Join(root, day.Format("2006/01/02")))
	}
	return dirs
}

type codexMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID        string    `json:"id"`
		Cwd       string    `json:"cwd"`
		Timestamp time.Time `json:"timestamp"`
		Source    any       `json:"source"`
	} `json:"payload"`
}

func readCodexMeta(path string) (codexMeta, bool) {
	var m codexMeta
	f, err := os.Open(path)
	if err != nil {
		return m, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		line = append(line, chunk...)
		if len(line) > maxMetaLine {
			return m, false
		}
		if err != nil || !isPrefix {
			break
		}
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" || m.Payload.ID == "" {
		return m, false
	}
	return m, true
}

// codexRollouts finds the rollout of an interactive Codex session in this
// cwd started within CodexWindow after the session; for a known ID (a
// restored generation), the rollout of that ID, wherever it is.
func codexRollouts(q Query) (string, error) {
	root := filepath.Join(codexHome(q), "sessions")
	if q.SessionID != "" {
		// A known ID (a restored generation resumes the conversation it was
		// given): its rollout, whatever day it started.
		matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*"+q.SessionID+".jsonl"))
		for _, path := range matches {
			if m, ok := readCodexMeta(path); ok && m.Payload.ID == q.SessionID {
				return q.SessionID, nil
			}
		}
		return "", notFound("no rollout for " + q.SessionID + " in " + root)
	}
	want := map[string]bool{}
	for _, c := range cwds(q.Cwd) {
		want[c] = true
	}
	// The directory date is local; a rollout we accept starts within
	// CodexWindow of the spawn, so look from the day before the start to the
	// day after the window's end.
	dirs := codexDayDirs(root, q.StartedAt, q.StartedAt.Add(CodexWindow), codexLocation)
	var cands []string
	for _, dir := range dedupe(dirs) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if !e.Type().IsRegular() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			if fi, err := e.Info(); err != nil || fi.ModTime().Before(q.StartedAt.Add(-mtimeSlack)) {
				continue // not written since the session started
			}
			m, ok := readCodexMeta(filepath.Join(dir, name))
			ts := m.Payload.Timestamp
			if !ok || !want[filepath.Clean(m.Payload.Cwd)] || ts.Before(q.StartedAt.Add(-time.Second)) || ts.After(q.StartedAt.Add(CodexWindow)) {
				continue
			}
			if s, ok := m.Payload.Source.(string); ok && s == "exec" {
				continue // a headless `codex exec` run, not a TUI
			}
			cands = append(cands, m.Payload.ID)
		}
	}
	return one(dedupe(cands), root)
}
