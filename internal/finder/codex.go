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
// session for this cwd that started after the Fastener session did.

func codexHome(env []string) string {
	if d := getenv(env, "CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(env), ".codex")
}

// maxMetaLine bounds how much of a rollout's first line is read: the meta
// record carries the base instructions, so it can be long, but it is never
// conversation content.
const maxMetaLine = 4 << 20

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
	for len(line) < maxMetaLine {
		chunk, isPrefix, err := r.ReadLine()
		line = append(line, chunk...)
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
// cwd started at or after the session (exact for an assigned ID: Codex
// cannot take one, but a profile may still name it).
func codexRollouts(q Query) (string, error) {
	root := filepath.Join(codexHome(q.Env), "sessions")
	want := map[string]bool{}
	for _, c := range cwds(q.Cwd) {
		want[c] = true
	}
	// The directory date is local; look from the day before the start (time
	// zones, midnight) to the day after now.
	var dirs []string
	for d := q.StartedAt.Add(-24 * time.Hour); !d.After(time.Now().Add(24 * time.Hour)); d = d.Add(24 * time.Hour) {
		dirs = append(dirs, filepath.Join(root, d.Local().Format("2006/01/02")))
	}
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
			if q.SessionID != "" && !strings.Contains(name, q.SessionID) {
				continue
			}
			if fi, err := e.Info(); err != nil || fi.ModTime().Before(q.StartedAt) {
				continue // not written since the session started
			}
			m, ok := readCodexMeta(filepath.Join(dir, name))
			if !ok || !want[filepath.Clean(m.Payload.Cwd)] || m.Payload.Timestamp.Before(q.StartedAt.Add(-time.Second)) {
				continue
			}
			if s, ok := m.Payload.Source.(string); ok && s == "exec" {
				continue // a headless `codex exec` run, not a TUI
			}
			if q.SessionID != "" && m.Payload.ID != q.SessionID {
				continue
			}
			cands = append(cands, m.Payload.ID)
		}
	}
	return one(dedupe(cands), root)
}
