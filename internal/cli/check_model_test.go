package cli

import (
	"strings"
	"testing"
)

func TestModelMatches(t *testing.T) {
	cases := []struct {
		expected, served string
		want             bool
	}{
		{"claude-haiku", "claude-haiku-4-5-20251001", true},
		{"claude-haiku-4.5", "claude-haiku-4-5-20251001", true},
		{"gpt-5", "gpt-5.6-luna", true},
		{"gpt-5", "gpt-50", false},
		{"sonnet", "claude-sonnet-5-5", false},
		{"flash-next", "glm-5.3-flash-next", false},
		{"github-copilot/claude-haiku", "openrouter/claude-haiku-4-5", false},
		{"github-copilot/claude-haiku-4.5", "github-copilot/claude-haiku-4-5-20251001", true},
		{"claude-haiku", "github-copilot/claude-haiku-4-5", true},
		{"glm", "glm-5.3-flash", true},
		{"qwen3.8", "qwen3-8-flash", true},
		// more
		{"github-copilot/claude-haiku", "claude-haiku-4-5", true},
		{"claude-opus-5-5", "claude-opus-5-5", true},
		{"Claude-Haiku", "claude-haiku-4-5", true},
		{"GitHub-Copilot/claude-haiku", "github-copilot/Claude-Haiku-4-5", true},
		{"haiku", "claude-haiku-4-5-20251001", false},
		{"claude-haiku-4.5", "claude-haiku-4-6", false},
		{"qwen3", "qwen3_8b", true},
		{"qwen3", "qwen3:8b", true},
		{"claude-haiku-", "claude-haiku-4-5", false},
		{"qwen3", "qwen38b", false},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5", false},
		{"openrouter/anthropic/claude-haiku", "openrouter/anthropic/claude-haiku-4.5", true},
		{"anthropic/claude-haiku", "openrouter/anthropic/claude-haiku-4.5", false},
	}
	for _, c := range cases {
		if got := modelMatches(c.expected, c.served); got != c.want {
			t.Errorf("modelMatches(%q, %q) = %v, want %v", c.expected, c.served, got, c.want)
		}
	}
}

func TestModelMismatchHint(t *testing.T) {
	cases := []struct{ expected, served, want string }{
		{"haiku", "claude-haiku-4-5-20251001", "--model claude-haiku"},
		{"flash-next", "glm-5.3-flash-next", "--model glm-5.3-flash-next"},
		{"sonnet", "claude-sonnet-5-5", "--expect-model claude-sonnet"},
		{"github-copilot/haiku", "github-copilot/claude-haiku-4-5", "--model github-copilot/claude-haiku"},
		{"gpt-5", "gpt-50", "the backend substituted a different model"},
		{"github-copilot/claude-haiku", "openrouter/claude-haiku-4-5", "the backend substituted a different model"},
		{"github-copilot/haiku", "openrouter/claude-haiku-4-5", "the backend substituted a different model"},
		{"opus", "claude-sonnet-5-5", "the backend substituted a different model"},
		{"claude-haiku-", "claude-haiku-4-5", "drop the trailing separator"},
		{"qwen3:", "qwen3:8b", "drop the trailing separator"},
		{"anthropic/claude-haiku", "openrouter/anthropic/claude-haiku-4.5", "the served provider is openrouter/anthropic; include it in --expect-model"},
	}
	for _, c := range cases {
		if got := modelMismatchHint(c.expected, c.served); !strings.Contains(got, c.want) {
			t.Errorf("modelMismatchHint(%q, %q) = %q, want it to contain %q", c.expected, c.served, got, c.want)
		}
	}
}

func TestModelMismatchHintNonASCII(t *testing.T) {
	// Case folding changes the byte length, so the suggestion comes from the
	// folded name.
	got := modelMismatchHint("haiku", "İclaude-haiku")
	want := "e.g. --model iclaude-haiku, or keep --model and pass --expect-model iclaude-haiku" // Go folds U+0130 to "i"
	if !strings.Contains(got, want) {
		t.Fatalf("hint %q, want it to contain %q", got, want)
	}
}

func TestModelStep(t *testing.T) {
	cases := []struct {
		expected, flag, served, source string
		status, detail                 string
	}{
		{"", "", "claude-opus-5-5", "server", stepPass, "served claude-opus-5-5"},
		{"", "", "", "", stepSkip, "backend does not report its model"},
		{"claude-haiku", "--model", "", "", stepSkip, "could not be verified"},
		{"claude-haiku", "--model", "claude-haiku-4-5", "server", stepPass, "requested claude-haiku, served claude-haiku-4-5"},
		{"glm", "--expect-model", "glm-5.3-flash-next", "server", stepPass, "expected glm, served glm-5.3-flash-next"},
		{"flash-next", "--model", "glm-5.3-flash-next", "server", stepFail, "requested flash-next, but the backend served glm-5.3-flash-next"},
		{"gpt-5", "--model", "p/gpt-5", "client", stepPass, "requested gpt-5, served p/gpt-5 (client-reported: the backend does not confirm the serving model, so a provider-side swap would not be detected)"},
		{"", "", "p/gpt-5", "client", stepPass, "served p/gpt-5 (client-reported"},
		{"claude", "--model", "p/gpt-5", "client", stepFail, "but the backend served p/gpt-5"},
	}
	for _, c := range cases {
		st := modelStep(c.expected, c.flag, c.served, c.source)
		if st.Name != "model" || st.Status != c.status || !strings.Contains(st.Detail, c.detail) ||
			(c.source != "client" && strings.Contains(st.Detail, "client-reported")) {
			t.Errorf("modelStep(%q, %q, %q) = %+v", c.expected, c.flag, c.served, st)
		}
		if (st.Status == stepFail) != (st.Hint != "") {
			t.Errorf("modelStep(%q, %q, %q): hint %q on status %s", c.expected, c.flag, c.served, st.Hint, st.Status)
		}
	}
}
