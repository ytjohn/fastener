package parser

import (
	"regexp"
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

// streamWindow is how much raw output StreamMatcher keeps between writes, so
// a match split across reads (or by escapes) is still found.
const streamWindow = 8 << 10

// StreamQuiet is how long output must have been quiet before a match that
// ends exactly at the end of the output so far is taken as complete.
const StreamQuiet = 500 * time.Millisecond

// StreamMatcher finds the latest match of a pattern in a terminal output
// stream, such as an interactive session's remote URL. It works on raw
// chunks as they are read: escapes are stripped (StripForMatch), a match
// may span chunks, and a match that reaches the end of what has been seen
// is held back until more output shows where it ends (or output has been
// quiet for StreamQuiet; see Flush), so a URL split across reads is never
// taken while cut short. Not safe for concurrent use.
type StreamMatcher struct {
	re       *regexp.Regexp
	tail     []byte
	pending  string    // a match that ended at the end of the output so far
	lastFeed time.Time // when pending was last seen
	now      func() time.Time
}

// NewStreamMatcher returns a matcher for re.
func NewStreamMatcher(re *regexp.Regexp) *StreamMatcher {
	return &StreamMatcher{re: re, now: time.Now}
}

// Feed adds raw output and returns the latest complete match in the window,
// or "" if there is none.
func (s *StreamMatcher) Feed(p []byte) string {
	buf := append(s.tail, p...)
	text := StripForMatch(buf)
	var found string
	s.pending = ""
	for _, loc := range s.re.FindAllIndex(text, -1) {
		m := string(text[loc[0]:loc[1]])
		if loc[1] == len(text) {
			s.pending, s.lastFeed = m, s.now()
			continue
		}
		found = m
	}
	if len(buf) > streamWindow {
		buf = buf[len(buf)-streamWindow:]
	}
	s.tail = append(s.tail[:0:0], buf...)
	return found
}

// Flush returns a match held back because it reached the end of the output,
// once output has been quiet for StreamQuiet since; else "".
func (s *StreamMatcher) Flush() string {
	if s.pending == "" || s.now().Sub(s.lastFeed) < StreamQuiet {
		return ""
	}
	m := s.pending
	s.pending = ""
	return m
}
