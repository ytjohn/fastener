package parser

import (
	"regexp"
	"slices"
)

// claudeRemote parses the status screen `claude remote-control` redraws on
// stdout (Claude Code 2.1.319). The screen is plain text with ANSI cursor and
// erase sequences, and each session line is an OSC 8 hyperlink:
//
//	ESC]8;;https://claude.ai/code/session_X?from=cli BEL Attached ESC]8;; BEL
//
// It records the bridge URL (https://claude.ai/code?environment=env_…) as
// RemoteURL, the latest one seen, and every session URL
// (https://claude.ai/code/session_…, without its query) in RemoteSessions, in
// the order first seen. Nothing on the screen is an error, so the parser never
// counts parse errors.
type claudeRemote struct{}

// maxRemoteSessions caps RemoteSessions so status.json stays small for a
// bridge that runs for weeks; the oldest entries are dropped first. A bridge
// serves at most 32 sessions at once by default.
const maxRemoteSessions = 64

var (
	// osc8RE matches an OSC 8 hyperlink opener or closer and captures its URI.
	// The terminator is BEL or ST (ESC \).
	osc8RE = regexp.MustCompile("\x1b\\]8;[^;\x07\x1b]*;([^\x07\x1b]*)(?:\x07|\x1b\\\\)")
	// escRE matches every other escape: other OSC strings, CSI sequences and
	// two-byte escapes.
	escRE = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b\\[[0-?]*[ -/]*[@-~]|\x1b[@-Z\\\\-_]")

	bridgeURLRE  = regexp.MustCompile(`https://claude\.ai/code\?environment=env_[A-Za-z0-9]+`)
	sessionURLRE = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9]+`)
)

// StripTerminal removes ANSI and OSC escape sequences from line. The URI of an
// OSC 8 hyperlink is kept, followed by a space, so links stay visible in plain
// text.
func StripTerminal(line []byte) []byte {
	line = osc8RE.ReplaceAllFunc(line, func(m []byte) []byte {
		sub := osc8RE.FindSubmatch(m)
		if len(sub[1]) == 0 {
			return nil // a closer
		}
		return append(append([]byte{}, sub[1]...), ' ')
	})
	return escRE.ReplaceAll(line, nil)
}

func (claudeRemote) Line(line []byte, f *Fields) bool {
	text := StripTerminal(line)
	changed := false
	if all := bridgeURLRE.FindAll(text, -1); len(all) > 0 {
		if u := string(all[len(all)-1]); u != f.RemoteURL {
			f.RemoteURL = u
			changed = true
		}
	}
	for _, m := range sessionURLRE.FindAll(text, -1) {
		u := string(m)
		if slices.Contains(f.RemoteSessions, u) {
			continue
		}
		// Copy before appending: a snapshot of Fields may share the old array.
		next := make([]string, 0, len(f.RemoteSessions)+1)
		next = append(next, f.RemoteSessions...)
		next = append(next, u)
		if len(next) > maxRemoteSessions {
			next = next[len(next)-maxRemoteSessions:]
		}
		f.RemoteSessions = next
		changed = true
	}
	return changed
}
