package finder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic stores only: bookkeeping lines and made-up cwds, never content.

func TestLocateClaude(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "proj.x")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	physical, _ := filepath.EvalSymlinks(cwd)
	q := Query{Env: []string{"HOME=" + home}, SessionID: idA}
	_, err := Locate("claude-transcripts", q)
	wantErr(t, err, ErrNotFound)

	// The cwd comes from the first line that has one, after bookkeeping
	// lines without it; a later line's cwd (a resume elsewhere) is not it.
	dir := filepath.Join(home, ".claude", "projects", claudeProjectKey(physical))
	path := filepath.Join(dir, idA+".jsonl")
	write(t, path, `{"type":"session-owner"}`+"\n"+`{"type":"queue-operation"}`+"\n"+
		`{"type":"user","cwd":"`+physical+`","x":1}`+"\n"+`{"type":"user","cwd":"/elsewhere"}`+"\n", time.Time{})
	locs, err := Locate("claude-transcripts", q)
	if err != nil || len(locs) != 1 {
		t.Fatalf("Locate: %+v %v", locs, err)
	}
	l := locs[0]
	if l.ID != idA || l.Path != path || l.Cwd != physical || l.ModTime.IsZero() {
		t.Fatalf("located %+v", l)
	}
	if !l.BelongsTo(cwd) || !l.BelongsTo(physical) || l.BelongsTo(t.TempDir()) {
		t.Fatal("BelongsTo")
	}

	// Another conversation's file is not it, and neither is a non-UUID.
	if _, err := Locate("claude-transcripts", Query{Env: q.Env, SessionID: idB}); err == nil {
		t.Fatal("found idB")
	}
	if _, err := Locate("claude-transcripts", Query{Env: q.Env, SessionID: "../x"}); err == nil || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := Locate("nope", q); err == nil {
		t.Fatal("unknown finder")
	}
}

func TestLocateClaudeCwdUnknown(t *testing.T) {
	conf := t.TempDir()
	q := Query{Env: []string{"CLAUDE_CONFIG_DIR=" + conf}, SessionID: idA}
	// No cwd field at all; and a cwd that does not name its directory.
	write(t, filepath.Join(conf, "projects", "-a-b", idA+".jsonl"), `{"type":"summary"}`+"\n", time.Time{})
	write(t, filepath.Join(conf, "projects", "-c-d", idA+".jsonl"), `{"cwd":"/somewhere/else"}`+"\n", time.Time{})
	locs, err := Locate("claude-transcripts", q)
	if err != nil || len(locs) != 2 {
		t.Fatalf("Locate: %+v %v", locs, err)
	}
	for _, l := range locs {
		if l.Cwd != "" {
			t.Fatalf("cwd %q from %s", l.Cwd, l.Path)
		}
	}
	if !locs[0].BelongsTo("/a/b") || locs[0].BelongsTo("/c/d") || !locs[1].BelongsTo("/c.d") {
		t.Fatal("BelongsTo by key")
	}
}

func TestFirstCwdSkipsLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	long := `{"cwd":"/long","pad":"` + strings.Repeat("x", maxCwdLineLen) + `"}`
	write(t, path, long+"\n"+`{"cwd":"/short"}`+"\n", time.Time{})
	if c := firstCwd(path, maxCwdLines); c != "/short" {
		t.Fatalf("firstCwd = %q", c)
	}
	write(t, path, `{"cwd":"relative"}`+"\n"+`{"cwd":7}`+"\n", time.Time{})
	if c := firstCwd(path, maxCwdLines); c != "" {
		t.Fatalf("firstCwd = %q", c)
	}
}

func TestLocatePi(t *testing.T) {
	agent := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	q := Query{Env: []string{"HOME=" + t.TempDir(), "PI_CODING_AGENT_DIR=" + agent}, SessionID: idA}
	_, err := Locate("pi-sessions", q)
	wantErr(t, err, ErrNotFound)
	path := filepath.Join(piSessionDir(q, physical), "2026-11-10T16-35-25-813Z_"+idA+".jsonl")
	write(t, path, `{"type":"session","version":3,"id":"`+idA+`","cwd":"`+physical+`"}`+"\n", time.Time{})
	locs, err := Locate("pi-sessions", q)
	if err != nil || len(locs) != 1 || locs[0].Path != path || locs[0].Cwd != physical || !locs[0].BelongsTo(cwd) || locs[0].BelongsTo(t.TempDir()) {
		t.Fatalf("Locate: %+v %v", locs, err)
	}
}

func TestLocateCodex(t *testing.T) {
	codex := t.TempDir()
	q := Query{Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}, SessionID: idA}
	_, err := Locate("codex-rollouts", q)
	wantErr(t, err, ErrNotFound)
	day := filepath.Join(codex, "sessions", "2026", "11", "01")
	// A file named for the ID whose meta names another is not it.
	write(t, filepath.Join(day, "rollout-x-"+idA+".jsonl"), codexMetaLine(idB, "/w", time.Now(), "cli"), time.Time{})
	_, err = Locate("codex-rollouts", q)
	wantErr(t, err, ErrNotFound)
	path := filepath.Join(day, "rollout-2026-11-01T10-00-00-"+idA+".jsonl")
	write(t, path, codexMetaLine(idA, "/w", time.Now(), "cli"), time.Time{})
	locs, err := Locate("codex-rollouts", q)
	if err != nil || len(locs) != 1 || locs[0].Path != path || locs[0].Cwd != "" || !locs[0].BelongsTo(t.TempDir()) {
		t.Fatalf("Locate: %+v %v", locs, err)
	}
}
