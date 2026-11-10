package monitor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"io"
	"regexp"

	"github.com/ytjohn/fastener/internal/parser"
)

// pumpDedupe is pump for a remote bridge, which redraws its status screen
// every few seconds: written through unchanged, output.log would grow without
// bound while saying nothing new (design §8.1). So:
//
//   - A line ends at "\n" or at a "\r" not followed by "\n" (a carriage-return
//     redraw), and every line is parsed.
//   - Each line is held until it ends, then written unless frameDedupe calls
//     it a repeat. A written line keeps its raw bytes, colours and OSC 8
//     links included, except that leading cursor-movement and erase
//     sequences are removed (see trimCursor), so `logs` on a terminal does
//     not erase earlier, unrelated lines. frameDedupe may also ask for a
//     "\n" to finish a row a "\r" left open (see frameDedupe).
//   - The held line is the one the parser accumulates anyway, so memory is
//     bounded the same way: a line that outgrows maxParseLine is written as
//     held, the rest streamed through raw, and it counts one parse error.
func (m *monitor) pumpDedupe(r io.Reader, out io.Writer, p parser.Parser) {
	br := bufio.NewReaderSize(r, 64*1024)
	fd := &frameDedupe{}
	var line []byte
	overlong := false
	pendingCR := false // the last byte read was "\r"; whether it ends the line depends on the next byte
	end := func(how lineEnd) {
		if overlong {
			fd.unique(how) // written already; nothing repeats it
		} else {
			a := fd.next(line, how)
			if a&actClose != 0 {
				_, _ = out.Write(newline)
			}
			if a&actLine != 0 {
				_, _ = out.Write(trimCursor(line))
			}
			if a&actNewline != 0 {
				_, _ = out.Write(newline)
			}
		}
		m.endLine(p, line, overlong)
		overlong = false
		line = resetLine(line)
	}
	add := func(seg []byte) {
		switch {
		case overlong:
			_, _ = out.Write(seg)
		case len(line)+len(seg) > maxParseLine:
			_, _ = out.Write(line) // stop holding: write what was held, then stream
			_, _ = out.Write(seg)
			overlong, line = true, nil
		default:
			line = appendCapped(line, seg)
			if len(line) > m.maxHeld {
				m.maxHeld = len(line)
			}
		}
	}
	for {
		chunk, err := br.ReadSlice('\n') // valid only until the next read
		if pendingCR && len(chunk) > 0 {
			pendingCR = false
			if chunk[0] != '\n' {
				end(endCR)
			}
		}
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\r')
			if i < 0 {
				add(chunk)
				break
			}
			add(chunk[:i+1])
			chunk = chunk[i+1:]
			if len(chunk) == 0 {
				pendingCR = true // decided by the next read
				break
			}
			if chunk[0] != '\n' {
				end(endCR) // a lone "\r"
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue // the line goes on
		case err == nil:
			end(endNL) // a newline
		default:
			if len(line) > 0 || overlong || pendingCR { // a last unterminated line
				end(endEOF)
			}
			return
		}
	}
}

var newline = []byte("\n")

// lineEnd is how a line ended.
type lineEnd int

const (
	endCR  lineEnd = iota // a lone "\r": the same row is drawn again
	endNL                 // "\n": the row is done
	endEOF                // the stream ended without a terminator
)

// cursorRE matches the cursor-movement and erase sequences (CSI n A-H, f, J,
// K) and cursor show/hide and other private modes (CSI ? n h/l) at the start
// of a line.
var cursorRE = regexp.MustCompile(`^(?:\x1b\[[0-9;]*[A-HJKf]|\x1b\[\?[0-9;]*[hl])+`)

// trimCursor drops leading cursor-movement and erase sequences from line.
func trimCursor(line []byte) []byte {
	if n := cursorRE.Find(line); n != nil {
		return line[len(n):]
	}
	return line
}

// markerRE finds, in a line's leading cursor sequences (cursorRE), one that
// moves the cursor up or home, which is how a redrawn frame starts:
// cursor-up (ESC[nA, Claude 2.1.319: ESC[nA ESC[J), cursor-previous-line
// (ESC[nF), cursor-position (ESC[H, ESC[r;cH, ESC[r;cf: full-screen ESC[H
// ESC[2J), and Ink's log-update (ESC[2K ESC[1A … ESC[2K ESC[G). Erase or
// column moves alone (ESC[2K, ESC[G) redraw the current row and are not
// frame starts.
var markerRE = regexp.MustCompile(`\x1b\[[0-9;]*[AFHf]`)

// isMarker reports whether text starts a redrawn frame.
func isMarker(text []byte) bool {
	n := cursorRE.Find(text)
	return n != nil && markerRE.Match(n)
}

// dedupeMaxLine: longer lines are never de-duplicated (always written), so
// the stripped copy hashed for each line stays small.
const dedupeMaxLine = 64 << 10

// dedupeMaxFrame caps how many rows of one frame are remembered; rows past
// it are compared with nothing and written. A Claude bridge's screen is about
// 8 lines plus one per session (32 by default).
const dedupeMaxFrame = 1024

// dedupeMaxPeriod is the longest block of lines recognised as repeating in
// output without frame markers.
const dedupeMaxPeriod = 64

// frameKey identifies a line by the SHA-256 of its text with terminal escapes
// stripped (redraws of the same line differ in cursor escapes). ok is false
// for a line that is never de-duplicated, so it equals nothing.
type frameKey struct {
	sum [sha256.Size]byte
	ok  bool
}

func (a frameKey) same(b frameKey) bool { return a.ok && b.ok && a.sum == b.sum }

// action says what to write for a line.
type action int

const (
	actClose   action = 1 << iota // a "\n" first, to finish a row a "\r" left open
	actLine                       // the line, minus leading cursor sequences
	actNewline                    // just a "\n": the row already shows this line's text
)

// frameDedupe decides which lines of a redrawn screen to write. Every line
// ending in "\n" is one row; the lines ending in a lone "\r" before it are
// earlier drawings of the same row (a spinner, a progress line), so a row's
// text is its last drawing; an empty drawing ("\r\r\n", a terminal's
// ONLCR) overwrites nothing, so it leaves the text as it was.
//
// With frame markers: a frame starts at a line whose leading cursor
// sequences move up or home (isMarker), and the rows before the first
// marker are a frame too. A row is a repeat when its text equals the row at
// the same position in the previous frame; a "\r" drawing is a repeat when
// it equals that row too, or the drawing before it in its row. So a frame is
// only ever compared with the frame right before it: a screen that changes
// and changes back (Connected, Disconnected, Connected) logs every change,
// and every row that differs from the previous frame is written.
//
// Without frame markers there are no frames to compare, so a line (row or
// "\r" drawing) is a repeat only when it completes a repeated block: for some
// period p ≤ dedupeMaxPeriod, the p lines ending with it equal the p lines
// before them. p = 1 is a line equal to the one before it; a 2-line status
// printed over and over, or a 4-step spinner, is written for one round and
// all but the last line of the next, then dropped until a line breaks the
// cycle; that line, any change to the block, is always written. Recent
// lines are not kept in a set: a line seen a while ago (A, B, A) is written
// again, so a return to an earlier state is never lost.
//
// Rows stay true and readable: once a "\r" drawing of a row was written, a
// repeat that the log's row does not show yet is written anyway (the row's
// final text always; with frame markers, any drawing), so the text the log
// shows last for a row is the screen's. When the log shows it already, a
// final "\n" is written alone to finish the row, so the next row never
// overwrites it; a frame marker after an open row writes "\n" first.
//
// Memory is two frames of row hashes (at most 2 × dedupeMaxFrame keys) and
// dedupeMaxPeriod hashes of recent lines.
type frameDedupe struct {
	framed    bool       // a frame marker has been seen
	prev, cur []frameKey // the rows of the previous and the current frame, in order
	drawn     frameKey   // the last "\r" drawing of the current row
	inRow     bool       // drawn is set: the current row has had "\r" drawings

	// Without frame markers: the recent lines, and run[p], how many lines
	// in a row have equalled the line p before them.
	hist [dedupeMaxPeriod]frameKey
	n    int
	run  [dedupeMaxPeriod + 1]int

	open  bool     // output.log's last write ended in a lone "\r"
	shown frameKey // what that open row shows
}

// next decides what to write for line, which ended as how, and records it.
func (d *frameDedupe) next(line []byte, how lineEnd) action {
	text := bytes.TrimRight(line, "\r\n")
	var key frameKey
	switch {
	case len(text) == 0 && d.inRow:
		// "\r" then nothing ("\r\r\n" from a terminal's ONLCR) overwrites
		// nothing: the row still shows its last drawing.
		key = d.drawn
	case len(text) <= dedupeMaxLine:
		key = frameKey{sum: sha256.Sum256(parser.StripTerminal(text)), ok: true}
	}
	return d.record(key, how, isMarker(text))
}

// unique records a line that repeats nothing and that nothing repeats (an
// overlong line, already written).
func (d *frameDedupe) unique(how lineEnd) { d.record(frameKey{}, how, false) }

func (d *frameDedupe) record(key frameKey, how lineEnd, marker bool) action {
	var a action
	if marker {
		if d.open {
			a |= actClose
			d.open = false
		}
		if d.inRow { // a row cut short by the redraw: its text is its last drawing
			d.endRow(d.drawn)
		}
		d.framed = true
		d.prev, d.cur = d.cur, d.prev[:0]
	}
	var dup bool
	if d.framed {
		pos := len(d.cur)
		dup = pos < len(d.prev) && d.prev[pos].same(key)
		if how == endCR && d.inRow && d.drawn.same(key) {
			dup = true
		}
	} else {
		dup = d.periodic(key)
	}
	switch {
	case !dup:
		a |= actLine
	case d.open && !d.shown.same(key) && (how != endCR || d.framed):
		// A repeat, but the log's open row shows another drawing of this
		// row: write it, so the row the log shows last is the screen's.
		// (A "\r" drawing without frame markers is dropped only as part of
		// a cycle already written, so it is not forced.)
		a |= actLine
	case d.open && how == endNL:
		a |= actNewline // the log shows this text already; finish the row
	}
	if a&(actLine|actNewline) != 0 {
		d.open = how == endCR && a&actLine != 0
		d.shown = key
	}
	if how == endCR {
		d.drawn, d.inRow = key, true
	} else {
		d.endRow(key)
	}
	return a
}

// endRow records the text of the current frame's next row.
func (d *frameDedupe) endRow(key frameKey) {
	d.inRow = false
	if len(d.cur) < dedupeMaxFrame {
		d.cur = append(d.cur, key)
	}
}

// periodic records key in the recent lines and reports whether it completes
// a repeated block of some period p: the p lines ending with it equal the p
// lines before them.
func (d *frameDedupe) periodic(key frameKey) bool {
	dup := false
	for p := 1; p <= dedupeMaxPeriod; p++ {
		if p <= d.n && d.hist[(d.n-p)%dedupeMaxPeriod].same(key) {
			d.run[p]++
			if d.run[p] >= p {
				dup = true
			}
		} else {
			d.run[p] = 0
		}
	}
	d.hist[d.n%dedupeMaxPeriod] = key
	d.n++
	return dup
}
