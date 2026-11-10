package finder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York below, whatever the host has
)

// Every store here is synthetic: empty or one-line files with made-up IDs.

func TestClaudeProjectKey(t *testing.T) {
	// Expected values computed with Claude Code 2.1.322's own function (in
	// Node) for the same strings.
	for in, want := range map[string]string{
		"/home/u/projects/audio.cpp":                           "-home-u-projects-audio-cpp",
		"/home/u/.config/x":                                    "-home-u--config-x",
		"/w/ü/a.b":                                             "-w---a-b",
		"/r/agent-harness/.claude/worktrees/v1":                "-r-agent-harness--claude-worktrees-v1",
		"/work/deep/" + strings.Repeat("segment-", 30) + "end": "-work-deep-" + strings.Repeat("segment-", 23) + "segme-c5ugdh",
		"/work/emoji-😀/" + strings.Repeat("x", 220):            "-work-emoji----" + strings.Repeat("x", 185) + "-pm927e",
	} {
		if got := claudeProjectKey(in); got != want {
			t.Errorf("claudeProjectKey(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func write(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

const (
	idA = "11111111-2222-4333-8444-555555555555"
	idB = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

func TestClaudeTranscripts(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "proj.x")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(cwd)
	dir := filepath.Join(home, ".claude", "projects", claudeProjectKey(real))
	start := time.Now().Add(-time.Minute)
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + home}, SessionID: idA}

	_, err := claudeTranscripts(q)
	wantErr(t, err, ErrNotFound) // not yet created: no message typed
	write(t, filepath.Join(dir, idB+".jsonl"), "{}\n", time.Time{})
	_, err = claudeTranscripts(q)
	wantErr(t, err, ErrNotFound) // another session's transcript is not ours
	write(t, filepath.Join(dir, idA+".jsonl"), "{}\n", time.Time{})
	if id, err := claudeTranscripts(q); err != nil || id != idA {
		t.Fatalf("assigned: %q %v", id, err)
	}

	// Without an assigned ID: the one transcript written since the start.
	q.SessionID = ""
	_, err = claudeTranscripts(q)
	wantErr(t, err, ErrAmbiguous) // two sessions in this cwd
	if err := os.Chtimes(filepath.Join(dir, idB+".jsonl"), start.Add(-time.Hour), start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if id, err := claudeTranscripts(q); err != nil || id != idA {
		t.Fatalf("heuristic: %q %v", id, err)
	}
}

func TestClaudeTranscriptsConfigDirAndSymlink(t *testing.T) {
	conf := t.TempDir()
	realCwd := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realCwd, link); err != nil {
		t.Fatal(err)
	}
	physical, _ := filepath.EvalSymlinks(realCwd)
	// Claude names the directory after its physical cwd, under
	// $CLAUDE_CONFIG_DIR (not ~/.claude) when that is set.
	write(t, filepath.Join(conf, "projects", claudeProjectKey(physical), idA+".jsonl"), "{}\n", time.Time{})
	q := Query{Cwd: link, StartedAt: time.Now().Add(-time.Minute), SessionID: idA,
		Env: []string{"HOME=" + t.TempDir(), "CLAUDE_CONFIG_DIR=" + conf}}
	if id, err := claudeTranscripts(q); err != nil || id != idA {
		t.Fatalf("CLAUDE_CONFIG_DIR + symlinked cwd: %q %v", id, err)
	}
	q.Env = q.Env[:1] // without the override: looks in ~/.claude, finds nothing
	_, err := claudeTranscripts(q)
	wantErr(t, err, ErrNotFound)
}

func TestPiSessions(t *testing.T) {
	agent := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	dir := piSessionDir(Query{Env: []string{"PI_CODING_AGENT_DIR=" + agent}}, physical)
	if want := filepath.Join(agent, "sessions", "--"+strings.ReplaceAll(strings.TrimPrefix(physical, "/"), "/", "-")+"--"); dir != want {
		t.Fatalf("dir %s, want %s", dir, want)
	}
	start := time.Date(2026, 11, 10, 16, 35, 0, 0, time.UTC)
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + t.TempDir(), "PI_CODING_AGENT_DIR=" + agent}, SessionID: idA}
	_, err := piSessions(q)
	wantErr(t, err, ErrNotFound)
	write(t, filepath.Join(dir, "2026-11-10T16-35-25-813Z_"+idA+".jsonl"), "{}\n", time.Time{})
	if id, err := piSessions(q); err != nil || id != idA {
		t.Fatalf("assigned: %q %v", id, err)
	}
	q.SessionID = ""
	write(t, filepath.Join(dir, "2026-11-10T16-36-00-001Z_"+idB+".jsonl"), "{}\n", time.Time{})
	_, err = piSessions(q)
	wantErr(t, err, ErrAmbiguous)
	q.StartedAt = start.Add(50 * time.Second) // after A was created
	if id, err := piSessions(q); err != nil || id != idB {
		t.Fatalf("heuristic: %q %v", id, err)
	}
}

func codexMetaLine(id, cwd string, ts time.Time, source string) string {
	return `{"timestamp":"` + ts.UTC().Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"` + id +
		`","timestamp":"` + ts.UTC().Format(time.RFC3339Nano) + `","cwd":"` + cwd + `","source":"` + source +
		`","base_instructions":{"text":"synthetic"}}}` + "\n" + `{"type":"event_msg","payload":{}}` + "\n"
}

func TestCodexRollouts(t *testing.T) {
	codex := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	start := time.Now().Add(-10 * time.Minute)
	day := filepath.Join(codex, "sessions", time.Now().Format("2006/01/02"))
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}}
	_, err := codexRollouts(q)
	wantErr(t, err, ErrNotFound) // no sessions dir at all yet
	// Before the session, another cwd, and a headless exec run: none count.
	write(t, filepath.Join(day, "rollout-old-"+idB+".jsonl"), codexMetaLine(idB, physical, start.Add(-time.Hour), "cli"), time.Time{})
	write(t, filepath.Join(day, "rollout-other-"+idB+".jsonl"), codexMetaLine(idB, "/elsewhere", start.Add(time.Minute), "cli"), time.Time{})
	write(t, filepath.Join(day, "rollout-exec-"+idB+".jsonl"), codexMetaLine(idB, physical, start.Add(time.Minute), "exec"), time.Time{})
	write(t, filepath.Join(day, "rollout-junk.jsonl"), "not json\n", time.Time{})
	_, err = codexRollouts(q)
	wantErr(t, err, ErrNotFound)
	write(t, filepath.Join(day, "rollout-new-"+idA+".jsonl"), codexMetaLine(idA, physical, start.Add(2*time.Minute), "cli"), time.Time{})
	if id, err := codexRollouts(q); err != nil || id != idA {
		t.Fatalf("one candidate: %q %v", id, err)
	}
	// A second interactive Codex in the same cwd since the start.
	const idC = "cccccccc-dddd-4eee-8fff-000000000000"
	write(t, filepath.Join(day, "rollout-new-"+idC+".jsonl"), codexMetaLine(idC, physical, start.Add(3*time.Minute), "cli"), time.Time{})
	_, err = codexRollouts(q)
	wantErr(t, err, ErrAmbiguous)
	if !strings.Contains(err.Error(), idA) || !strings.Contains(err.Error(), idC) {
		t.Fatalf("the ambiguity does not name both: %v", err)
	}
}

func TestGet(t *testing.T) {
	for _, n := range []string{"claude-transcripts", "codex-rollouts", "pi-sessions"} {
		if _, ok := Get(n); !ok {
			t.Errorf("%s missing", n)
		}
	}
	if _, ok := Get("nope"); ok {
		t.Error("unknown finder found")
	}
}

// TestCodexOutsideWindow (scenario A): a Codex the user starts by hand in
// the same folder long after the session's spawn is not taken for it.
func TestCodexOutsideWindow(t *testing.T) {
	codex := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	start := time.Now().Add(-40 * time.Minute)
	day := filepath.Join(codex, "sessions", time.Now().Format("2006/01/02"))
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}}
	write(t, filepath.Join(day, "rollout-late-"+idB+".jsonl"), codexMetaLine(idB, physical, start.Add(30*time.Minute), "cli"), time.Time{})
	_, err := codexRollouts(q)
	wantErr(t, err, ErrNotFound)
	write(t, filepath.Join(day, "rollout-ok-"+idA+".jsonl"), codexMetaLine(idA, physical, start.Add(5*time.Minute), "cli"), time.Time{})
	if id, err := codexRollouts(q); err != nil || id != idA {
		t.Fatalf("within the window: %q %v", id, err)
	}
}

// TestCodexDayDirsAcrossDST: the day directories are calendar dates, so the
// short day at a DST change is not skipped.
func TestCodexDayDirsAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 7, 23, 30, 0, 0, ny) // DST starts 2026-03-08 02:00
	got := codexDayDirs("/r", start, start.Add(25*time.Hour), ny)
	want := []string{"/r/2026/03/06", "/r/2026/03/07", "/r/2026/03/08", "/r/2026/03/09", "/r/2026/03/10"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("dirs %v, want %v", got, want)
	}
	// And the finder finds a rollout dated the short day.
	old := codexLocation
	codexLocation = ny
	t.Cleanup(func() { codexLocation = old })
	codex := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	start = time.Date(2026, 3, 7, 23, 58, 0, 0, ny)
	ts := time.Date(2026, 3, 8, 0, 5, 0, 0, ny) // within the window, on the next (short) day
	write(t, filepath.Join(codex, "sessions", "2026/03/08", "rollout-x-"+idA+".jsonl"), codexMetaLine(idA, physical, ts, "cli"), time.Time{})
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}}
	if id, err := codexRollouts(q); err != nil || id != idA {
		t.Fatalf("rollout on the DST day: %q %v", id, err)
	}
}

// TestRelativeStoreDirs: a relative CLAUDE_CONFIG_DIR, CODEX_HOME or
// PI_CODING_AGENT_DIR is relative to the agent's cwd.
func TestRelativeStoreDirs(t *testing.T) {
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	q := Query{Cwd: physical, StartedAt: time.Now().Add(-time.Minute), SessionID: idA,
		Env: []string{"HOME=/nonexistent", "CLAUDE_CONFIG_DIR=rel/claude", "PI_CODING_AGENT_DIR=rel/pi", "CODEX_HOME=rel/codex"}}
	write(t, filepath.Join(physical, "rel/claude/projects", claudeProjectKey(physical), idA+".jsonl"), "{}\n", time.Time{})
	if id, err := claudeTranscripts(q); err != nil || id != idA {
		t.Fatalf("claude: %q %v", id, err)
	}
	write(t, filepath.Join(piSessionDir(q, physical), "2026-11-10T16-35-25-813Z_"+idA+".jsonl"), "{}\n", time.Time{})
	if !strings.HasPrefix(piSessionDir(q, physical), filepath.Join(physical, "rel/pi")) {
		t.Fatalf("pi dir %s", piSessionDir(q, physical))
	}
	if id, err := piSessions(q); err != nil || id != idA {
		t.Fatalf("pi: %q %v", id, err)
	}
	if got := codexHome(q); got != filepath.Join(physical, "rel/codex") {
		t.Fatalf("codex home %s", got)
	}
}

// TestCoarseMtimeSlack: a file whose mtime (rounded down to the second by
// the filesystem) is just before StartedAt still counts.
func TestCoarseMtimeSlack(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	start := time.Now().Truncate(time.Second).Add(700 * time.Millisecond)
	dir := filepath.Join(home, ".claude", "projects", claudeProjectKey(physical))
	write(t, filepath.Join(dir, idA+".jsonl"), "{}\n", start.Truncate(time.Second))
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + home}}
	if id, err := claudeTranscripts(q); err != nil || id != idA {
		t.Fatalf("%q %v", id, err)
	}
}

// TestCodexKnownID: a restored generation's known ID finds its rollout in
// whatever day directory it started, with no time window.
func TestCodexKnownID(t *testing.T) {
	codex := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	long := time.Now().AddDate(0, 0, -20)
	write(t, filepath.Join(codex, "sessions", long.Format("2006/01/02"), "rollout-x-"+idA+".jsonl"), codexMetaLine(idA, physical, long, "cli"), time.Time{})
	q := Query{Cwd: cwd, StartedAt: time.Now(), SessionID: idA, Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}}
	if id, err := codexRollouts(q); err != nil || id != idA {
		t.Fatalf("%q %v", id, err)
	}
	q.SessionID = idB
	_, err := codexRollouts(q)
	wantErr(t, err, ErrNotFound)
}

// TestInvalidIDsNeverReturned: a store that names a conversation with
// something that is not an ID (here a flag) never yields it, alone or among
// candidates, and an invalid assigned ID is refused.
func TestInvalidIDsNeverReturned(t *testing.T) {
	const bad = "--dangerously-skip-permissions"
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	codexF, _ := Get("codex-rollouts")
	codex := t.TempDir()
	cwd := t.TempDir()
	physical, _ := filepath.EvalSymlinks(cwd)
	start := time.Now().Add(-time.Minute)
	day := filepath.Join(codex, "sessions", time.Now().Format("2006/01/02"))
	write(t, filepath.Join(day, "rollout-x-evil.jsonl"), codexMetaLine(bad, physical, start.Add(time.Second), "cli"), time.Time{})
	q := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}, Logf: logf}
	if id, err := codexF(q); id != "" || !errors.Is(err, ErrNotFound) {
		t.Fatalf("a bad rollout id was returned: %q %v", id, err)
	}
	write(t, filepath.Join(day, "rollout-x-"+idA+".jsonl"), codexMetaLine(idA, physical, start.Add(2*time.Second), "cli"), time.Time{})
	if id, err := codexF(q); err != nil || id != idA {
		t.Fatalf("the good one beside a bad one: %q %v", id, err)
	}
	if len(logged) == 0 {
		t.Fatal("the invalid id was not logged")
	}

	piF, _ := Get("pi-sessions")
	agent := t.TempDir()
	pq := Query{Cwd: cwd, StartedAt: start, Env: []string{"HOME=" + t.TempDir(), "PI_CODING_AGENT_DIR=" + agent}}
	write(t, filepath.Join(piSessionDir(pq, physical), "2026-11-10T16-35-25-813Z_"+bad+".jsonl"), "{}\n", time.Time{})
	pq.StartedAt = time.Date(2026, 11, 10, 16, 35, 0, 0, time.UTC)
	if id, err := piF(pq); id != "" || err == nil {
		t.Fatalf("a bad pi session id was returned: %q %v", id, err)
	}

	claudeF, _ := Get("claude-transcripts")
	if _, err := claudeF(Query{Cwd: cwd, SessionID: bad, Env: []string{"HOME=" + t.TempDir()}}); err == nil || !strings.Contains(err.Error(), "invalid session ID") {
		t.Fatalf("an invalid assigned ID: %v", err)
	}
	if ValidID(bad) || !ValidID(idA) || !ValidID("01a1c6a7-3a9a-70c0-844b-3bcb82d854fb") {
		t.Fatal("ValidID")
	}
}
