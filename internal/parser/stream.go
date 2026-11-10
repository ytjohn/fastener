package parser

import (
	"regexp"
	"strings"
	"time"
)

// cursorMoveRE matches CSI sequences that move the cursor or erase (CUU,
// CUD, CUF, CUB, CNL, CPL, CHA, CUP, ED, EL, VPA, HVP …). A TUI such as
// Claude Code's draws spaces and line breaks with them, so for matching they
// separate words: each becomes a space.
var cursorMoveRE = regexp.MustCompile("\x1b\\[[0-9;?]*[A-HJKSTdf]")

// StripForMatch is StripTerminal for matching text on a redrawn screen:
// cursor movements become spaces (instead of vanishing), so words the screen
// shows apart stay apart, while attribute changes (SGR) inside a word still
// vanish, and an OSC 8 link's URI is kept.
func StripForMatch(b []byte) []byte {
	return StripTerminal(cursorMoveRE.ReplaceAll(b, []byte(" ")))
}

// StreamOverlap is how much stripped text StreamMatcher keeps from earlier
// output, so a match split across reads (or by escapes) is still found. It
// bounds the longest match it can find, which is ample for a URL.
const StreamOverlap = 512

// maxHeldEscape bounds how much of an unfinished escape sequence at the end
// of a read is held for the next one; a longer one is not an escape the
// stripper would remove anyway (or is malformed), so it is processed as is.
const maxHeldEscape = 4 << 10

// StreamQuiet is how long output must have been quiet before a match that
// ends exactly at the end of the output so far is taken as complete.
const StreamQuiet = 500 * time.Millisecond

// StreamMatcher finds the latest match of a pattern in a terminal output
// stream, such as an interactive session's remote URL. It works on raw
// chunks as they are read and incrementally: each byte is stripped once
// (StripForMatch; an escape sequence cut by a read is held until it ends),
// and the pattern runs over the new text plus StreamOverlap bytes before it.
//
// Two sources, in order of preference:
//   - the URI of an OSC 8 hyperlink that matches the pattern: once one has
//     been seen, visible-text matches are ignored, since the visible text of
//     a link can be wrapped across rows on a narrow terminal while its URI
//     is whole;
//   - otherwise the visible text. A match that reaches the end of what has
//     been seen is held back until more output shows where it ends (or the
//     output has been quiet for StreamQuiet; see Flush), so a URL split
//     across reads is never taken while cut short.
//
// Not safe for concurrent use.
type StreamMatcher struct {
	re       *regexp.Regexp
	held     []byte    // raw bytes of an escape sequence cut by the last read
	text     []byte    // the last StreamOverlap bytes of stripped text
	sawLink  bool      // an OSC 8 URI matched: visible text no longer counts
	pending  string    // a visible match that ended at the end of the output so far
	lastFeed time.Time // when pending was last seen
	now      func() time.Time
}

// NewStreamMatcher returns a matcher for re.
func NewStreamMatcher(re *regexp.Regexp) *StreamMatcher {
	return &StreamMatcher{re: re, now: time.Now}
}

// Feed adds raw output and returns the latest complete match it shows, or
// "" if there is none.
func (s *StreamMatcher) Feed(p []byte) string {
	raw := p
	if len(s.held) > 0 {
		raw = append(s.held, p...)
		s.held = nil
	}
	if cut := unfinishedEscape(raw); cut >= 0 && len(raw)-cut <= maxHeldEscape {
		s.held = append([]byte(nil), raw[cut:]...)
		raw = raw[:cut]
	}
	if len(raw) == 0 {
		return ""
	}
	var found string
	for _, m := range osc8RE.FindAllSubmatch(raw, -1) {
		if u := s.re.Find(m[1]); u != nil {
			found, s.sawLink = string(u), true
		}
	}
	keep := len(s.text)
	s.text = append(s.text, StripForMatch(raw)...)
	if !s.sawLink {
		s.pending = ""
		for _, loc := range s.re.FindAllIndex(s.text, -1) {
			if loc[1] < keep {
				continue // ended before the new text: already reported
			}
			m := string(s.text[loc[0]:loc[1]])
			if loc[1] == len(s.text) {
				s.pending, s.lastFeed = m, s.now()
				continue
			}
			found = m
		}
	} else {
		s.pending = ""
	}
	if n := len(s.text); n > StreamOverlap {
		s.text = append(s.text[:0:0], s.text[n-StreamOverlap:]...)
	}
	return found
}

// Flush returns a visible-text match held back because it reached the end
// of the output, once output has been quiet for StreamQuiet since; else "".
func (s *StreamMatcher) Flush() string {
	if s.pending == "" || s.now().Sub(s.lastFeed) < StreamQuiet {
		return ""
	}
	m := s.pending
	s.pending = ""
	return m
}

// unfinishedEscape returns the index of an escape sequence in b that has not
// ended by the end of b, or -1. Only the last ESC can start one: an earlier
// sequence either ended or was cut by a later ESC.
func unfinishedEscape(b []byte) int {
	i := strings.LastIndexByte(string(b), 0x1b)
	if i < 0 {
		return -1
	}
	if escapeEnded(b[i:]) {
		return -1
	}
	return i
}

// escapeEnded reports whether the escape sequence at the start of b (b[0]
// is ESC) is complete within b, by the forms the stripper knows: CSI ends at
// a final byte 0x40–0x7E; OSC, DCS, SOS, PM and APC strings at BEL or ST
// (ESC \, which a later ESC would be, so an ESC inside means it ended);
// other escapes at the first byte after their intermediates (0x20–0x2F).
func escapeEnded(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	switch b[1] {
	case '[':
		for _, c := range b[2:] {
			if c >= 0x40 && c <= 0x7e {
				return true
			}
		}
		return false
	case ']', 'P', 'X', '^', '_':
		for _, c := range b[2:] {
			if c == 0x07 {
				return true
			}
		}
		return false
	default:
		for _, c := range b[1:] {
			if c < 0x20 || c > 0x2f {
				return true
			}
		}
		return false
	}
}
