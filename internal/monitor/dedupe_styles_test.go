package monitor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The redraw styles of issue #60. Each fixture is a status screen redrawn
// many times, with one state change two thirds of the way through; the
// output.log must keep both states. Bytes in → output.log, before and after
// #60 (go test -v -run 'RedrawStyles|RealCapture'):
//
//	style                       raw       before    after
//	ESC[H ESC[2J full screen    23,781    21,674      103
//	Ink log-update (2K/1A)      33,261    21,600      103
//	markerless 2-line block     17,500    17,500   17,500  (by design, below)
//	4-step "\r" spinner         49,600    49,600   49,600  (by design, below)
//	Claude 2.1.319 capture       4,351     1,684    1,684  (TestPumpDedupeRealCapture)
//
// Without frame markers only a line equal to the one before it is dropped:
// collapsing a repeating block would leave the log on a stale state when
// the stream stops part-way through a cycle (see frameDedupe).
type redrawStyle struct {
	name string
	// frame renders redraw i of a screen showing state; prevLines is the
	// number of lines the previous frame drew (0 for the first).
	frame  func(i int, state string, prevLines int) (string, int)
	frames int
	max    int // the most output.log may hold
	// want: every one of these must be in output.log.
	want []string
}

var redrawStyles = []redrawStyle{
	{
		name: "full-screen ESC[H ESC[2J",
		frame: func(i int, state string, _ int) (string, int) {
			return "\x1b[H\x1b[2J" + state + " · example · main\n    Capacity: 0/32\n\nspace to show QR code\n", 4
		},
		frames: 301, max: 160,
		want: []string{"Connected · example", "Disconnected · example", "Capacity: 0/32\n", "space to show QR code\n"},
	},
	{
		// Ink's log-update erases each line of the previous frame from the
		// bottom up (ESC[2K ESC[1A … ESC[2K ESC[G) and writes the new frame,
		// which it ends with "\n".
		name: "Ink log-update",
		frame: func(i int, state string, prev int) (string, int) {
			var b strings.Builder
			if prev > 0 {
				for range prev {
					b.WriteString("\x1b[2K\x1b[1A")
				}
				b.WriteString("\x1b[2K\x1b[G")
			}
			b.WriteString(state + " · example · main\n    Capacity: 0/32\n\nspace to show QR code\n")
			return b.String(), 4
		},
		frames: 300, max: 160,
		want: []string{"Connected · example", "Disconnected · example", "Capacity: 0/32\n", "space to show QR code\n"},
	},
	{
		name: "markerless 2-line block",
		frame: func(i int, state string, _ int) (string, int) {
			return fmt.Sprintf("bridge: %-12s\nqueue: 0 jobs\n", state), 2
		},
		frames: 500, max: 17500, // nothing is dropped

		want: []string{"bridge: Connected", "bridge: Disconnected", "queue: 0 jobs\n"},
	},
	{
		name: "4-step \\r spinner",
		frame: func(i int, state string, _ int) (string, int) {
			glyph := []string{"|", "/", "-", "\\"}[i%4]
			return fmt.Sprintf("%s %-28s\r", glyph, state+" to the bridge"), 1
		},
		frames: 1600, max: 49600, // nothing is dropped
		want: []string{"Connected to the bridge", "Disconnected to the bridge"},
	},
}

// stateAt: the screen says Connected, then Disconnected from two thirds of
// the way through.
func stateAt(i, n int) string {
	if i >= 2*n/3 {
		return "Disconnected"
	}
	return "Connected"
}

func TestPumpDedupeRedrawStyles(t *testing.T) {
	for _, st := range redrawStyles {
		t.Run(st.name, func(t *testing.T) {
			var in strings.Builder
			prev := 0
			var last string
			for i := range st.frames {
				last, prev = st.frame(i, stateAt(i, st.frames), prev)
				in.WriteString(last)
			}
			m := newTestMonitor(t)
			m.plan.DedupeFrames = true
			rp := &recordingParser{p: mustParser(t, "none")}
			var out bytes.Buffer
			m.pump(strings.NewReader(in.String()), &out, rp)
			got := out.String()
			t.Logf("%s: %d bytes in, %d in output.log:\n%q", st.name, in.Len(), out.Len(), got)
			if out.Len() > st.max {
				t.Errorf("output.log is %d bytes (%d raw); want at most %d", out.Len(), in.Len(), st.max)
			}
			for _, w := range st.want {
				if !strings.Contains(got, w) {
					t.Errorf("output.log lacks %q", w)
				}
			}
			if rp.lines < st.frames {
				t.Errorf("parser saw %d lines for %d frames", rp.lines, st.frames)
			}
		})
	}
}

func TestIsMarker(t *testing.T) {
	for _, c := range []struct {
		line string
		want bool
	}{
		{"\x1b[7A\x1b[Jstatus", true},           // Claude 2.1.319
		{"\x1b[A status", true},                 // cursor up, no count
		{"\x1b[3Fstatus", true},                 // cursor previous line
		{"\x1b[H\x1b[2Jstatus", true},           // full screen
		{"\x1b[2J\x1b[Hstatus", true},           // erase first
		{"\x1b[1;1Hstatus", true},               // cursor position
		{"\x1b[1;1fstatus", true},               // HVP
		{"\x1b[2K\x1b[1A\x1b[2K\x1b[Gst", true}, // Ink log-update
		{"\x1b[?25l\x1b[2A\x1b[Jstatus", true},  // hidden cursor first
		{"\x1b[0m\x1b[2Astatus", true},          // SGR first
		{"\x1b[1;32m\x1b[2K\x1b[3Fst", true},    // SGR and erase first
		{"\x1b8\x1b[Jstatus", true},             // restore the cursor
		{"\x1b[ustatus", true},                  // restore (SCO)
		{"\x1b7status", false},                  // save only
		{"\x1b[sstatus", false},                 // save only (SCO)
		{"\x1b[0mstatus", false},                // colour only
		{"\x1b[>5ustatus", false},               // kitty keyboard flags, not restore
		{"\x1b]0;t\x07\x1b[2Astatus", false},    // an OSC first: not scanned
		{"\x1b[2Kstatus", false},                // erase the current row only
		{"\x1b[Gstatus", false},                 // column 1
		{"\x1b[2K\x1b[Gstatus", false},          // the same row again
		{"\x1b[1Bstatus", false},                // cursor down
		{"\x1b[Jstatus", false},
		{"status\x1b[1A", false}, // not leading
		{"\x1b[1mbold\x1b[1A", false},
		{"plain", false},
	} {
		if got := isMarker([]byte(c.line)); got != c.want {
			t.Errorf("isMarker(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestPumpDedupeMarkerlessStateChanges: without frame markers, every line
// that differs from the line before it is written, so a stream that stops
// part-way through a repeating block still ends on its last state.
func TestPumpDedupeMarkerlessStateChanges(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		// Review of #77: the period rule logged only the first three lines.
		{"flapping", "Disconnected\nReconnected\nDisconnected\nReconnected\n", "Disconnected\nReconnected\nDisconnected\nReconnected\n"},
		// Review of #77: the period rule dropped the final "state 3".
		{"cycle of three", "state 1\nstate 2\nstate 3\nstate 1\nstate 2\nstate 3\n", "state 1\nstate 2\nstate 3\nstate 1\nstate 2\nstate 3\n"},
		{"drawings flapping", "Disconnected\rReconnected\rDisconnected\rReconnected\r", "Disconnected\rReconnected\rDisconnected\rReconnected\r"},
		{"repeats dropped", "Connected\nConnected\nDisconnected\nDisconnected\nConnected\n", "Connected\nDisconnected\nConnected\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newTestMonitor(t)
			m.plan.DedupeFrames = true
			var out bytes.Buffer
			m.pump(strings.NewReader(c.in), &out, mustParser(t, "none"))
			if got := out.String(); got != c.want {
				t.Errorf("output.log =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

// TestPumpDedupeCarriageReturnRows: a row's final text is what the log
// shows for it, even when it repeats.
func TestPumpDedupeCarriageReturnRows(t *testing.T) {
	spin := func(n int, text string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString([]string{"|", "/", "-", "\\"}[i%4] + " " + text + "\r")
		}
		return b.String()
	}
	for _, c := range []struct{ name, in, want string }{
		// Without frame markers a spinner's drawings all differ from the
		// one before them, so all are written, and the row ends on its text.
		{"spinner ends on new text", spin(6, "wait") + "done\n",
			"| wait\r/ wait\r- wait\r\\ wait\r| wait\r/ wait\rdone\n"},
		{"spinner ends on its last drawing", spin(6, "wait") + "/ wait\nnext\n",
			"| wait\r/ wait\r- wait\r\\ wait\r| wait\r/ wait\r\nnext\n"},
		// A redraw with nothing new: the row is finished with "\n" alone.
		{"same text redrawn", strings.Repeat("ready\r", 50) + "ready\nnext\n", "ready\r\nnext\n"},
		// Frames compare a row's final text, however it was drawn.
		{"framed rows", "Connecting\rConnected\nCapacity: 0/32\n" +
			"\x1b[2A\x1b[JConnected\nCapacity: 0/32\n" +
			"\x1b[2A\x1b[JReconnecting\rConnected\nCapacity: 0/32\n",
			"Connecting\rConnected\nCapacity: 0/32\nReconnecting\rConnected\n"},
		// A drawing equal to the previous frame's row is written once the
		// log shows another drawing of the row.
		{"framed drawing back", "Connected\n" + "\x1b[1A\x1b[JLost\rConnected\r" + "\x1b[1A\x1b[JConnected\n",
			"Connected\nLost\rConnected\r\n"},
		// An empty drawing overwrites nothing: "\r\r\n" (a pty's ONLCR)
		// ends a row that still shows its text.
		{"CR CR LF rows", "a\r\r\nb\r\r\n\x1b[2A\x1b[Ja\r\r\nc\r\r\n", "a\r\nb\r\nc\r\r\n"},
		// Review of #77: an SGR before the cursor-up hid the frame start, so
		// the change back to a, b was compared with stale rows and lost.
		{"SGR before the marker", "\x1b[2Aa\nb\n" + "\x1b[0m\x1b[2Ac\nd\n" + "\x1b[2Aa\nb\n", "a\nb\n\x1b[0mc\nd\na\nb\n"},
		// A frame start that is still not recognised (here after an OSC
		// title) merges two frames; the next marker moves up over fewer
		// rows than that, so its frame is compared with nothing.
		{"missed frame start", "\x1b[2Aa\nb\n" + "\x1b]0;t\x07\x1b[2Ac\nd\n" + "\x1b[2Aa\nb\n",
			"a\nb\n\x1b]0;t\x07\x1b[2Ac\nd\na\nb\n"},
		// Restoring a cursor saved above the screen starts a frame.
		{"save and restore", "\x1b7a\nb\n\x1b8\x1b[Ja\nb\n\x1b8\x1b[Ja\nc\n", "a\nb\nc\n"},
		// A frame marker after an open row finishes it first.
		{"marker after open row", "a\nb\r\x1b[2A\x1b[Ja\nc\n", "a\nb\r\nc\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newTestMonitor(t)
			m.plan.DedupeFrames = true
			var out bytes.Buffer
			m.pump(strings.NewReader(c.in), &out, mustParser(t, "none"))
			if got := out.String(); got != c.want {
				t.Errorf("output.log =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}
