package parser

import (
	"slices"
	"testing"
)

// testdata/claude-remote.txt is a sanitized real capture of
// `claude remote-control --name N` stdout (Claude Code 2.1.319): the
// environment and session ids are fixed fakes and the folder is "example".
// It covers connecting, a session attaching and being named, and shutdown.
func TestClaudeRemoteFixture(t *testing.T) {
	p, ok := Get("claude-remote")
	if !ok {
		t.Fatal("claude-remote not registered")
	}
	got := feed(t, p, "testdata/claude-remote.txt")
	if want := "https://claude.ai/code?environment=env_01FAKEFAKEFAKEFAKEFAKEFAKE"; got.RemoteURL != want {
		t.Errorf("remote_url = %q, want %q", got.RemoteURL, want)
	}
	want := []string{"https://claude.ai/code/session_01FAKEFAKEFAKEFAKEFAKE01"}
	if !slices.Equal(got.RemoteSessions, want) {
		t.Errorf("remote_sessions = %q, want %q", got.RemoteSessions, want)
	}
	if got.ParseErrors != 0 || got.ConversationID != "" || got.FinalMessage != "" {
		t.Errorf("unexpected fields: %+v", got)
	}
}

func TestClaudeRemoteLines(t *testing.T) {
	p, _ := Get("claude-remote")
	var f Fields
	steps := []struct {
		line     string
		changed  bool
		url      string
		sessions []string
	}{
		{"\x1b[1A\x1b[J·✔︎· Connected · example · main", false, "", nil},
		{"Continue in https://claude.ai/code?environment=env_A1", true, "https://claude.ai/code?environment=env_A1", nil},
		{"Continue in https://claude.ai/code?environment=env_A1", false, "https://claude.ai/code?environment=env_A1", nil},
		// OSC 8 with an ST terminator; the query is dropped.
		{"    \x1b]8;;https://claude.ai/code/session_S1?from=cli\x1b\\Attached\x1b]8;;\x1b\\", true,
			"https://claude.ai/code?environment=env_A1", []string{"https://claude.ai/code/session_S1"}},
		// BEL terminator, two links on one line, one already known.
		{"\x1b]8;;https://claude.ai/code/session_S2?from=cli\x07two\x1b]8;;\x07 \x1b]8;;https://claude.ai/code/session_S1\x07one\x1b]8;;\x07", true,
			"https://claude.ai/code?environment=env_A1", []string{"https://claude.ai/code/session_S1", "https://claude.ai/code/session_S2"}},
		// A new environment replaces the old one.
		{"https://claude.ai/code?environment=env_B2", true,
			"https://claude.ai/code?environment=env_B2", []string{"https://claude.ai/code/session_S1", "https://claude.ai/code/session_S2"}},
		{"", false, "https://claude.ai/code?environment=env_B2", []string{"https://claude.ai/code/session_S1", "https://claude.ai/code/session_S2"}},
	}
	for i, s := range steps {
		if c := p.Line([]byte(s.line), &f); c != s.changed {
			t.Errorf("step %d: changed = %v, want %v", i, c, s.changed)
		}
		if f.RemoteURL != s.url || !slices.Equal(f.RemoteSessions, s.sessions) {
			t.Errorf("step %d: got %q %q, want %q %q", i, f.RemoteURL, f.RemoteSessions, s.url, s.sessions)
		}
	}
}

func TestClaudeRemoteSessionCap(t *testing.T) {
	p, _ := Get("claude-remote")
	var f Fields
	for i := 0; i < maxRemoteSessions+10; i++ {
		p.Line([]byte("https://claude.ai/code/session_"+string(rune('a'+i%26))+string(rune('a'+i/26))), &f)
	}
	if len(f.RemoteSessions) != maxRemoteSessions {
		t.Fatalf("len = %d, want %d", len(f.RemoteSessions), maxRemoteSessions)
	}
	if f.RemoteSessions[0] != "https://claude.ai/code/session_ka" {
		t.Errorf("oldest kept = %q", f.RemoteSessions[0])
	}
}

func TestStripTerminal(t *testing.T) {
	in := "\x1b[8A\x1b[J\x1b[1;32mok\x1b[0m \x1b]8;;https://x/y\x07label\x1b]8;;\x07 \x1b]0;title\x07end"
	if got, want := string(StripTerminal([]byte(in))), "ok https://x/y label end"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
