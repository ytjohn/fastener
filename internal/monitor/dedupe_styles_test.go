package monitor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The redraw styles of issue #60. Each fixture is a status screen redrawn
// many times, with one state change two thirds of the way through; the
// output.log must keep both states and be small. Bytes in → output.log,
// before and after #60 (go test -v -run 'RedrawStyles|RealCapture'):
//
//	style                       raw       before    after
//	ESC[H ESC[2J full screen    23,781    21,674      103
//	Ink log-update (2K/1A)      33,261    21,600      103
//	markerless 2-line block     17,500    17,500       91
//	4-step "\r" spinner         49,600    49,600      465
//	Claude 2.1.319 capture       4,351     1,684    1,684  (TestPumpDedupeRealCapture)
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
		frames: 500, max: 160,
		want: []string{"bridge: Connected", "bridge: Disconnected", "queue: 0 jobs\n"},
	},
	{
		name: "4-step \\r spinner",
		frame: func(i int, state string, _ int) (string, int) {
			glyph := []string{"|", "/", "-", "\\"}[i%4]
			return fmt.Sprintf("%s %-28s\r", glyph, state+" to the bridge"), 1
		},
		frames: 1600, max: 512,
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

// TestPumpDedupeMarkerlessStateChanges: without frame markers a repeating
// block is dropped, but a change and a change back are both written.
func TestPumpDedupeMarkerlessStateChanges(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	var in strings.Builder
	for _, st := range []string{"Connected", "Connected", "Connected", "Disconnected", "Disconnected", "Connected", "Connected"} {
		in.WriteString("·✔︎· " + st + " · example\n    Capacity: 0/32\n\nspace to show QR code\n")
	}
	var out bytes.Buffer
	m.pump(strings.NewReader(in.String()), &out, mustParser(t, "none"))
	got := out.String()
	if n := strings.Count(got, " Connected "); n != 3 {
		// once, its repeat's first line (a block is dropped from its second
		// round's last line on), and after the disconnect
		t.Errorf("Connected logged %d times, want 3:\n%q", n, got)
	}
	if n := strings.Count(got, " Disconnected "); n != 1 {
		t.Errorf("Disconnected logged %d times, want 1:\n%q", n, got)
	}
	if strings.LastIndex(got, " Connected ") < strings.LastIndex(got, " Disconnected ") {
		t.Errorf("the change back to Connected is not logged after Disconnected:\n%q", got)
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
		// The final drawing is part of the dropped cycle, but the log shows
		// another drawing: it is written, with its "\n".
		{"spinner ends on a cycle drawing", spin(40, "wait") + "\\ wait\nnext\n",
			"| wait\r/ wait\r- wait\r\\ wait\r| wait\r/ wait\r- wait\r\\ wait\nnext\n"},
		{"spinner ends on new text", spin(40, "wait") + "done\n",
			"| wait\r/ wait\r- wait\r\\ wait\r| wait\r/ wait\r- wait\rdone\n"},
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
