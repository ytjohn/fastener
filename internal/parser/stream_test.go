package parser

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var claudeSessionRE = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9_-]+`)

const sessURL = "https://claude.ai/code/session_017sf5rxuW4AeHi7B5SRz7ij"

func feedAll(s *StreamMatcher, chunks ...string) string {
	var last string
	for _, c := range chunks {
		if m := s.Feed([]byte(c)); m != "" {
			last = m
		}
	}
	return last
}

func TestStreamMatcherWhole(t *testing.T) {
	// What Claude Code 2.1.322 draws: spaces as cursor-forward moves, the URL
	// as an OSC 8 link with ?from=cli, then a box-drawing rule right after.
	line := "\x1b[2C/remote-control\x1b[1Cis\x1b[1Cactive\x1b[1C·\x1b[1Cor\x1b[1Cat\x1b[3C" +
		"\x1b]8;;" + sessURL + "?from=cli\x07" + sessURL + "\x1b]8;;\x07\x1b[39m────────"
	if got := feedAll(NewStreamMatcher(claudeSessionRE), line); got != sessURL {
		t.Fatalf("got %q", got)
	}
}

func TestStreamMatcherSplitAcrossReads(t *testing.T) {
	s := NewStreamMatcher(claudeSessionRE)
	half := len(sessURL) - 7
	if got := s.Feed([]byte("at " + sessURL[:half])); got != "" {
		t.Fatalf("took a cut URL: %q", got)
	}
	if got := s.Feed([]byte(sessURL[half:] + " \r\n")); got != sessURL {
		t.Fatalf("got %q", got)
	}
}

func TestStreamMatcherSplitInsideEscape(t *testing.T) {
	// An SGR change inside the URL, itself split across reads.
	full := "at \x1b[1mhttps://claude.ai/code/\x1b[0;3" + "6msession_017sf5rxuW4AeHi7B5SRz7ij\x1b[0m\r\n"
	for cut := 1; cut < len(full); cut++ {
		s := NewStreamMatcher(claudeSessionRE)
		if got := feedAll(s, full[:cut], full[cut:]); got != sessURL {
			t.Fatalf("cut at %d: got %q", cut, got)
		}
	}
}

func TestStreamMatcherHeldUntilQuiet(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewStreamMatcher(claudeSessionRE)
	s.now = func() time.Time { return now }
	if got := s.Feed([]byte("at " + sessURL)); got != "" {
		t.Fatalf("took a URL at the end of the output at once: %q", got)
	}
	now = now.Add(StreamQuiet / 2)
	if got := s.Flush(); got != "" {
		t.Fatalf("flushed before the output was quiet: %q", got)
	}
	now = now.Add(StreamQuiet)
	if got := s.Flush(); got != sessURL {
		t.Fatalf("Flush after quiet = %q", got)
	}
	if got := s.Flush(); got != "" {
		t.Fatalf("flushed twice: %q", got)
	}
}

func TestStreamMatcherLatestAndWindow(t *testing.T) {
	s := NewStreamMatcher(claudeSessionRE)
	other := strings.Replace(sessURL, "017", "999", 1)
	if got := feedAll(s, sessURL+"\n", strings.Repeat("x", 3*streamWindow), other+"\n"); got != other {
		t.Fatalf("got %q", got)
	}
	if len(s.tail) > streamWindow {
		t.Fatalf("window grew to %d", len(s.tail))
	}
	s.Feed([]byte(strings.Repeat("y", 2*streamWindow))) // pushes it out of the window
	if got := s.Feed([]byte("z")); got != "" {
		t.Fatalf("a URL outside the window still matched: %q", got)
	}
}

func TestStripForMatchSeparatesCursorMoves(t *testing.T) {
	if got := string(StripForMatch([]byte("Quick\x1b[1Csafety\x1b[1Ccheck\x1b[2;5HIs\x1b[1mthis"))); got != "Quick safety check Isthis" {
		t.Fatalf("got %q", got)
	}
}
