package finder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	dir := piSessionDir([]string{"PI_CODING_AGENT_DIR=" + agent}, physical)
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
