package monitor

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// A generated redrawn screen: frames of rows, each row drawn zero or more
// times with a lone "\r" before its final text.
type genRow struct {
	draws []string // earlier drawings, each ending in "\r"
	text  string   // final text
	cut   bool     // the row ends at its last drawing, cut short by the next frame's marker
}

// shows is the row's text on a terminal: its final text, or its last
// drawing when the final text is empty ("\r" then nothing overwrites
// nothing).
func (r genRow) shows() string {
	if r.text == "" && len(r.draws) > 0 {
		return r.draws[len(r.draws)-1]
	}
	return r.text
}

var (
	genTexts = []string{"Connected", "Disconnected", "Capacity: 0/32", "Capacity: 1/32", "", "space to show QR code"}
	genDraws = []string{"| Connecting", "/ Connecting", "Connected", "Disconnected"}
)

// genFrames builds frames from a stream of choices (math/rand, or fuzz input
// bytes). Frame 0 is drawn without a marker and has distinct rows, so the
// markerless rule never drops one; every later frame starts with a marker.
func genFrames(pick func(n int) int, nFrames int, draws bool) [][]genRow {
	frames := [][]genRow{nil}
	for r := range 1 + pick(4) {
		frames[0] = append(frames[0], genRow{text: fmt.Sprintf("boot %d", r)})
	}
	for range nFrames {
		var f []genRow
		for range 1 + pick(5) {
			row := genRow{text: genTexts[pick(len(genTexts))]}
			if draws {
				for range pick(3) {
					row.draws = append(row.draws, genDraws[pick(len(genDraws))])
				}
			}
			f = append(f, row)
		}
		if last := &f[len(f)-1]; len(last.draws) > 0 && pick(2) == 0 {
			last.cut = true
			last.text = last.draws[len(last.draws)-1]
		}
		frames = append(frames, f)
	}
	return frames
}

// genMarker is one of the redraw styles' frame starts, for a previous frame
// of prev rows.
func genMarker(style, prev int) string {
	switch style % 5 {
	case 0:
		return fmt.Sprintf("\x1b[%dA\x1b[J", prev) // Claude 2.1.319
	case 1:
		return "\x1b[H\x1b[2J"
	case 2:
		return strings.Repeat("\x1b[2K\x1b[1A", prev) + "\x1b[2K\x1b[G" // Ink
	case 3:
		return fmt.Sprintf("\x1b[%dF", prev)
	default:
		return fmt.Sprintf("\x1b[?25l\x1b[%dA\x1b[J", prev)
	}
}

// genItem is one line as pumpDedupe splits them.
type genItem struct {
	line  string
	how   lineEnd
	frame int
	row   int
	last  bool // the row's last line
}

func genItems(frames [][]genRow, style func() int) []genItem {
	var items []genItem
	for fi, f := range frames {
		prefix := ""
		if fi > 0 {
			prefix = genMarker(style(), len(frames[fi-1]))
		}
		for ri, row := range f {
			draws := row.draws
			if row.cut {
				draws = draws[:len(draws)-1]
			}
			for _, d := range draws {
				items = append(items, genItem{prefix + d + "\r", endCR, fi, ri, false})
				prefix = ""
			}
			if row.cut {
				items = append(items, genItem{prefix + row.text + "\r", endCR, fi, ri, true})
			} else {
				items = append(items, genItem{prefix + row.text + "\n", endNL, fi, ri, true})
			}
			prefix = ""
		}
	}
	return items
}

// checkFrames feeds frames to frameDedupe and checks that nothing the
// screen showed is lost: after every frame, the text the log shows last for
// each of its rows is that row's final text; a row of the first frame, and
// any row that differs from the previous frame's row at its position, is
// written in that frame; and a row left open by a "\r" is finished before
// another row is written.
func checkFrames(t *testing.T, frames [][]genRow, style func() int) {
	t.Helper()
	items := genItems(frames, style)
	d := &frameDedupe{}
	view := map[int]string{}   // row → the text the log shows last for it
	wrote := map[[2]int]bool{} // (frame, row) → a line of it was written
	openRow := -1              // the row the log has open after a "\r", or -1
	openFrame := -1
	for _, it := range items {
		a := d.next([]byte(it.line), it.how)
		if a&actClose != 0 || a&actNewline != 0 {
			openRow = -1
		}
		if a&actLine != 0 {
			if openRow >= 0 && (openRow != it.row || openFrame != it.frame) {
				t.Fatalf("frame %d row %d written onto open row %d of frame %d\nframes: %+v", it.frame, it.row, openRow, openFrame, frames)
			}
			// What a terminal shows: the written text, except that an empty
			// line after a "\r" on the same row overwrites nothing.
			text := string(trimCursor([]byte(strings.TrimRight(it.line, "\r\n"))))
			if text != "" || openRow != it.row || openFrame != it.frame {
				view[it.row] = text
			}
			wrote[[2]int{it.frame, it.row}] = true
			openRow, openFrame = -1, -1
			if it.how == endCR {
				openRow, openFrame = it.row, it.frame
			}
		}
		if !it.last { // a row is checked once its last line is handled
			continue
		}
		f, r := it.frame, it.row
		want := frames[f][r].shows()
		changed := f == 0 || r >= len(frames[f-1]) || frames[f-1][r].shows() != want
		if changed && !wrote[[2]int{f, r}] {
			t.Fatalf("frame %d row %d (%q) changed but was not written\nframes: %+v", f, r, want, frames)
		}
		if view[r] != want {
			t.Fatalf("after frame %d row %d the log shows %q, the screen %q\nframes: %+v", f, r, view[r], want, frames)
		}
	}
}

// checkPump runs drawing-free frames through the pump: output.log is
// exactly the first frame plus every changed row, in order.
func checkPump(t *testing.T, frames [][]genRow, style func() int) {
	t.Helper()
	var in strings.Builder
	var want []string
	for _, it := range genItems(frames, style) {
		in.WriteString(it.line)
	}
	for fi, f := range frames {
		for ri, row := range f {
			if fi == 0 || ri >= len(frames[fi-1]) || frames[fi-1][ri].text != row.text {
				want = append(want, row.text+"\n")
			}
		}
	}
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	var out bytes.Buffer
	m.pump(strings.NewReader(in.String()), &out, mustParser(t, "none"))
	if got := out.String(); got != strings.Join(want, "") {
		t.Fatalf("output.log =\n%q\nwant\n%q", got, strings.Join(want, ""))
	}
}

// TestDedupeFramesNeverLoseState: random frame sequences in every redraw
// style, with and without "\r" drawings, never lose a state, the last frame
// included.
func TestDedupeFramesNeverLoseState(t *testing.T) {
	for seed := range uint64(3000) {
		r := rand.New(rand.NewPCG(seed, 60))
		pick := func(n int) int { return r.IntN(n) }
		style := func() int { return r.IntN(5) }
		draws := seed%2 == 1
		frames := genFrames(pick, 1+r.IntN(12), draws)
		checkFrames(t, frames, style)
		if !draws && seed%10 == 0 {
			checkPump(t, frames, style)
		}
	}
}

// FuzzDedupeFrames is TestDedupeFramesNeverLoseState driven by fuzz input:
// go test -fuzz FuzzDedupeFrames ./internal/monitor.
func FuzzDedupeFrames(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, true)
	f.Add([]byte{3, 3, 3, 0, 1, 0, 1, 0, 1, 2, 2, 2, 5, 4, 3, 2, 1}, false)
	f.Add(bytes.Repeat([]byte{1, 4, 2, 0, 2, 1}, 20), true)
	f.Fuzz(func(t *testing.T, data []byte, draws bool) {
		i := 0
		pick := func(n int) int {
			if i >= len(data) {
				return 0
			}
			i++
			return int(data[i-1]) % n
		}
		frames := genFrames(pick, len(data)/4, draws)
		checkFrames(t, frames, func() int { return pick(5) })
	})
}

// TestDedupeMarkerlessDropsOnlyCycles: without frame markers, a line is
// dropped exactly when, for some period p, the p lines ending with it equal
// the p lines before them (checked by brute force on random input).
func TestDedupeMarkerlessDropsOnlyCycles(t *testing.T) {
	for seed := range uint64(300) {
		r := rand.New(rand.NewPCG(seed, 61))
		// Repeat random blocks a random number of times, so cycles of many
		// periods occur, and break them now and then.
		var lines []string
		for len(lines) < 400 {
			block := make([]string, 1+r.IntN(8))
			for i := range block {
				block[i] = string(rune('a' + r.IntN(4)))
			}
			for range 1 + r.IntN(5) {
				lines = append(lines, block...)
			}
		}
		d := &frameDedupe{}
		open, shown := false, "" // output.log's row left open by a "\r", and what it shows
		for i, l := range lines {
			how := endNL
			if r.IntN(3) == 0 {
				how = endCR
			}
			term := map[lineEnd]string{endNL: "\n", endCR: "\r"}[how]
			a := d.next([]byte(l+term), how)
			// A row's final text is forced out when the open row shows
			// another drawing; else a line is dropped exactly on a cycle.
			forced := how == endNL && open && shown != l
			wantLine := !cycle(lines[:i+1]) || forced
			wantNL := !wantLine && how == endNL && open
			if got := a&actLine != 0; got != wantLine || (a&actNewline != 0) != wantNL {
				t.Fatalf("seed %d line %d (%q%s): action %b, want line %v newline %v\n%q", seed, i, l, term, a, wantLine, wantNL, lines[max(0, i-20):i+1])
			}
			switch {
			case a&actLine != 0:
				open, shown = how == endCR, l
			case a&actNewline != 0:
				open = false
			}
		}
	}
}

// cycle reports whether the last line of lines completes a repeated block.
func cycle(lines []string) bool {
	n := len(lines)
	for p := 1; p <= dedupeMaxPeriod && 2*p <= n; p++ {
		if slices.Equal(lines[n-p:], lines[n-2*p:n-p]) {
			return true
		}
	}
	return false
}
