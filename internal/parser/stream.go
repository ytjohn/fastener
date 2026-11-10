package parser

import (
	"bytes"
	"regexp"
	"slices"
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
// (StripForMatch; an escape sequence or string cut by a read is held until
// it ends), and the pattern runs over the new text plus StreamOverlap bytes
// before it. The stripped text is the same as StripForMatch over the whole
// stream would give.
//
// Matches are taken in the order they appear (an OSC 8 link's URI is part of
// the stripped text, where the link is), with two rules for the visible text
// of a link, which a narrow terminal can wrap across rows while its URI
// stays whole:
//   - a match that is a strict prefix of the last link URI that matched is a
//     fragment of that link and is ignored;
//   - a visible match (not a link URI) that is a strict prefix of the last
//     match reported is a cut-short copy and is ignored; a link URI is always
//     taken.
//
// A match that reaches the end of what has been seen is held back until more
// output shows where it ends (or the output has been quiet for StreamQuiet;
// see Flush), so a URL split across reads is never taken while cut short.
//
// Not safe for concurrent use.
type StreamMatcher struct {
	re       *regexp.Regexp
	held     []byte    // raw bytes of an escape sequence cut by the last read
	text     []byte    // the last StreamOverlap bytes of stripped text
	links    []string  // link URIs (matches) seen in the current and last few reads
	lastLink string    // the last link URI that matched
	last     string    // the last match reported
	pending  string    // a match that ended at the end of the output so far
	lastFeed time.Time // when pending was last seen
	now      func() time.Time

	// onText, for tests, receives every piece of stripped text.
	onText func([]byte)
}

// NewStreamMatcher returns a matcher for re.
func NewStreamMatcher(re *regexp.Regexp) *StreamMatcher {
	return &StreamMatcher{re: re, now: time.Now}
}

// Feed adds raw output and returns the latest match it completes that the
// rules above accept, or "" if there is none.
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
	for _, m := range osc8RE.FindAllSubmatch(raw, -1) {
		if u := s.re.Find(m[1]); u != nil {
			s.lastLink = string(u)
			s.links = append(s.links, s.lastLink)
		}
	}
	if len(s.links) > 8 {
		s.links = s.links[len(s.links)-8:]
	}
	stripped := StripForMatch(raw)
	if s.onText != nil {
		s.onText(stripped)
	}
	keep := len(s.text)
	s.text = append(s.text, stripped...)
	var found string
	s.pending = ""
	for _, loc := range s.re.FindAllIndex(s.text, -1) {
		if loc[1] < keep {
			continue // ended before the new text: already handled
		}
		m := string(s.text[loc[0]:loc[1]])
		if loc[1] == len(s.text) {
			s.pending, s.lastFeed = m, s.now()
			continue
		}
		if s.accept(m) {
			found = m
		}
	}
	if n := len(s.text); n > StreamOverlap {
		s.text = append(s.text[:0:0], s.text[n-StreamOverlap:]...)
	}
	return found
}

// accept applies the rules for a completed match and records it.
func (s *StreamMatcher) accept(m string) bool {
	if m != s.lastLink && strings.HasPrefix(s.lastLink, m) {
		return false // a fragment of a link's (wrapped) visible text
	}
	if m != s.last && strings.HasPrefix(s.last, m) && !slices.Contains(s.links, m) {
		return false // a cut-short visible copy of what was reported
	}
	s.last = m
	return true
}

// Flush returns a match held back because it reached the end of the
// output, once output has been quiet for StreamQuiet since (and the rules
// accept it); else "".
func (s *StreamMatcher) Flush() string {
	if s.pending == "" || s.now().Sub(s.lastFeed) < StreamQuiet {
		return ""
	}
	m := s.pending
	s.pending = ""
	if !s.accept(m) {
		return ""
	}
	return m
}

// unfinishedEscape returns the index from which the end of b may still be
// part of an escape sequence or string the stripper removes as one unit, or
// -1. A string (OSC, DCS, SOS, PM, APC: ESC then ], P, X, ^ or _) runs to BEL
// or ST (ESC \) and may contain other ESC bytes, so the last opener whose
// string has not ended is found first; otherwise only the last ESC can start
// an unfinished sequence.
func unfinishedEscape(b []byte) int {
	if j := lastStringOpener(b); j >= 0 && !stringEnded(b[j+2:]) {
		return j
	}
	i := bytes.LastIndexByte(b, 0x1b)
	if i < 0 {
		return -1
	}
	if escapeEnded(b[i:]) {
		return -1
	}
	return i
}

// lastStringOpener is the index of the last ESC ], P, X, ^ or _ in b, or -1.
func lastStringOpener(b []byte) int {
	for i := len(b) - 2; i >= 0; i-- {
		if b[i] == 0x1b {
			switch b[i+1] {
			case ']', 'P', 'X', '^', '_':
				return i
			}
		}
	}
	return -1
}

// stringEnded reports whether a string body (after its opener) has ended:
// with BEL, or with ESC followed by anything (ST, ESC \, ends it; any other
// byte ends the stripper's match there too). An ESC as the last byte may
// still become ST.
func stringEnded(body []byte) bool {
	for i, c := range body {
		switch c {
		case 0x07:
			return true
		case 0x1b:
			return i+1 < len(body)
		}
	}
	return false
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
