package parser

import (
	"math/rand"
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
	if got := feedAll(s, sessURL+"\n", strings.Repeat("x", 3*StreamOverlap), other+"\n"); got != other {
		t.Fatalf("got %q", got)
	}
	if len(s.text) > StreamOverlap {
		t.Fatalf("kept %d bytes of text", len(s.text))
	}
	// A URL already reported is not reported again as more output arrives.
	if got := s.Feed([]byte("more")); got != "" {
		t.Fatalf("reported again: %q", got)
	}
}

// TestStreamMatcherPrefersLinkURI: a link whose visible text wraps across
// rows (a narrow terminal) yields the URI, never the first row's fragment.
func TestStreamMatcherPrefersLinkURI(t *testing.T) {
	wrapped := "\x1b]8;;" + sessURL + "?from=cli\x07" + sessURL[:40] + "\r\n\x1b[2C" + sessURL[40:] + "\x1b]8;;\x07\r\n"
	for cut := 0; cut <= len(wrapped); cut++ {
		s := NewStreamMatcher(claudeSessionRE)
		var got []string
		for _, c := range []string{wrapped[:cut], wrapped[cut:], "later output\r\n"} {
			if m := s.Feed([]byte(c)); m != "" {
				got = append(got, m)
			}
		}
		for _, m := range got {
			if m != sessURL {
				t.Fatalf("cut %d: reported %q (all: %q)", cut, m, got)
			}
		}
		if len(got) == 0 {
			t.Fatalf("cut %d: nothing reported", cut)
		}
	}
	// Once a link has matched, visible text alone no longer counts.
	s := NewStreamMatcher(claudeSessionRE)
	s.Feed([]byte(wrapped))
	if got := s.Feed([]byte(sessURL[:45] + "\r\n")); got != "" {
		t.Fatalf("visible fragment taken after a link: %q", got)
	}
}

// TestStreamMatcherRandomSplits: however the output is cut into reads, the
// matcher reports the same URL, and never a cut-short one.
func TestStreamMatcherRandomSplits(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	streams := []string{
		"\x1b[2Cat\x1b[1C\x1b[1m" + sessURL + "\x1b[0m\x1b[3C───\r\n",
		"at \x1b[1mhttps://claude.ai/code/\x1b[0;36msession_017sf5rxuW4AeHi7B5SRz7ij\x1b[0m\r\n",
		"\x1b]8;;" + sessURL + "?from=cli\x07" + sessURL + "\x1b]8;;\x07\x1b[39m────",
		strings.Repeat("\x1b[38;5;220mnoise\x1b[39m\x1b[12G", 200) + " " + sessURL + " \x1bPtmux;\x1b\x1b]11;?\x07\x1b\\ tail",
	}
	for si, stream := range streams {
		for trial := 0; trial < 2000; trial++ {
			s := NewStreamMatcher(claudeSessionRE)
			var got []string
			for rest := stream; len(rest) > 0; {
				n := 1 + rng.Intn(min(len(rest), 40))
				if m := s.Feed([]byte(rest[:n])); m != "" {
					got = append(got, m)
				}
				rest = rest[n:]
			}
			if m := s.Feed([]byte(" ")); m != "" {
				got = append(got, m)
			}
			if len(got) == 0 {
				t.Fatalf("stream %d trial %d: nothing", si, trial)
			}
			for _, m := range got {
				if m != sessURL {
					t.Fatalf("stream %d trial %d: %q", si, trial, got)
				}
			}
		}
	}
}

func TestStripForMatchSeparatesCursorMoves(t *testing.T) {
	if got := string(StripForMatch([]byte("Quick\x1b[1Csafety\x1b[1Ccheck\x1b[2;5HIs\x1b[1mthis"))); got != "Quick safety check Isthis" {
		t.Fatalf("got %q", got)
	}
}
