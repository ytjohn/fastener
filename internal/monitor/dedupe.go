package monitor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"io"
	"regexp"
	"strconv"

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

// leadRE matches the run of escape sequences a redraw puts at the start of a
// line before its text: cursor movement and erase (CSI n A-H, f, J, K),
// SGR (CSI n m), private modes such as cursor show/hide (CSI ? n h/l), and
// cursor save and restore (CSI s, CSI u, ESC 7, ESC 8).
var leadRE = regexp.MustCompile(`^(?:\x1b\[[0-9;]*[A-HJKf]|\x1b\[[0-9;:]*m|\x1b\[\?[0-9;]*[hl]|\x1b\[[su]|\x1b[78])+`)

// leadSeqRE splits a leading run into its sequences; leadSGRRE is an SGR.
var (
	leadSeqRE = regexp.MustCompile(`\x1b\[[0-9;:?]*[A-HJKfmsuhl]|\x1b[78]`)
	leadSGRRE = regexp.MustCompile(`^\x1b\[[0-9;:]*m$`)
)

// trimCursor drops the leading run's cursor-movement, erase, mode and
// save/restore sequences from line, so `logs` on a terminal does not erase
// or overwrite earlier, unrelated lines. SGR sequences (colours) are kept.
func trimCursor(line []byte) []byte {
	run := leadRE.Find(line)
	if run == nil {
		return line
	}
	var sgr []byte
	for _, seq := range leadSeqRE.FindAll(run, -1) {
		if leadSGRRE.Match(seq) {
			sgr = append(sgr, seq...)
		}
	}
	if sgr == nil {
		return line[len(run):]
	}
	return append(sgr, line[len(run):]...)
}

// frameStart describes a line's leading run (leadRE): whether it starts a
// redrawn frame, by moving the cursor up or home, and by how many rows it
// moves up. Frame starts: cursor-up (ESC[nA; Claude 2.1.319: ESC[nA ESC[J),
// cursor-previous-line (ESC[nF), cursor-position (ESC[H, ESC[r;cH,
// ESC[r;cf: full-screen ESC[H ESC[2J), Ink's log-update (ESC[2K ESC[1A …
// ESC[2K ESC[G), and cursor restore (ESC 8, CSI u) back to a position saved
// above the frame. SGR, modes, erase, save and column moves (ESC[2K, ESC[G)
// may come before or between them, and alone start no frame. up counts the
// rows of ESC[nA and ESC[nF; counted is false when a move is absolute.
type frameStart struct {
	marker, counted bool
	up              int
}

func leadFrame(text []byte) frameStart {
	run := leadRE.Find(text)
	if run == nil {
		return frameStart{}
	}
	fs := frameStart{counted: true}
	for _, seq := range leadSeqRE.FindAll(run, -1) {
		final := seq[len(seq)-1]
		switch {
		case len(seq) == 2: // ESC 7 or ESC 8
			if final == '8' {
				fs.marker, fs.counted = true, false
			}
		case seq[2] == '?':
		case final == 'A' || final == 'F':
			n, _ := strconv.Atoi(string(seq[2 : len(seq)-1]))
			fs.marker, fs.up = true, fs.up+max(n, 1)
		case final == 'H' || final == 'f' || final == 'u':
			fs.marker, fs.counted = true, false
		}
	}
	return fs
}

// isMarker reports whether text starts a redrawn frame.
func isMarker(text []byte) bool { return leadFrame(text).marker }

// dedupeMaxLine: longer lines are never de-duplicated (always written), so
// the stripped copy hashed for each line stays small.
const dedupeMaxLine = 64 << 10

// dedupeMaxFrame caps how many rows of one frame are remembered; rows past
// it are compared with nothing and written. A Claude bridge's screen is about
// 8 lines plus one per session (32 by default).
const dedupeMaxFrame = 1024

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
// With frame markers: a frame starts at a line whose leading sequences move
// the cursor up or home (leadFrame), and the rows before the first marker
// are a frame too. A row is a repeat when its text equals the row at the
// same position in the previous frame; a "\r" drawing is a repeat when it
// equals that row too, or the drawing before it in its row. So a frame is
// only ever compared with the frame right before it: a screen that changes
// and changes back (Connected, Disconnected, Connected) logs every change,
// and every row that differs from the previous frame is written. If a
// marker moves up over fewer rows than the frame it ends holds, a frame
// start was missed and the rows are not aligned, so the new frame is
// compared with nothing and written whole.
//
// Without frame markers there are no frames, and a line (row or drawing) is
// a repeat only when it equals the line right before it. Nothing longer is
// collapsed: a stream that stops part-way through a repeating block would
// otherwise leave the log showing a state that is no longer the screen's.
//
// Rows stay true and readable: once a "\r" drawing of a row was written, a
// repeat that the log's row does not show yet is written anyway, so the
// text the log shows last for a row is the screen's. When the log shows it
// already, a final "\n" is written alone to finish the row, so the next row
// never overwrites it; a frame marker after an open row writes "\n" first.
//
// Memory is two frames of row hashes (at most 2 × dedupeMaxFrame keys).
type frameDedupe struct {
	framed    bool       // a frame marker has been seen
	prev, cur []frameKey // the rows of the previous and the current frame, in order
	drawn     frameKey   // the last "\r" drawing of the current row
	inRow     bool       // drawn is set: the current row has had "\r" drawings
	last      frameKey   // the line before this one (without frame markers)

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
		run := leadRE.Find(text) // ESC 7 and ESC 8 survive StripTerminal
		key = frameKey{sum: sha256.Sum256(parser.StripTerminal(text[len(run):])), ok: true}
	}
	return d.record(key, how, leadFrame(text))
}

// unique records a line that repeats nothing and that nothing repeats (an
// overlong line, already written).
func (d *frameDedupe) unique(how lineEnd) { d.record(frameKey{}, how, frameStart{}) }

func (d *frameDedupe) record(key frameKey, how lineEnd, fs frameStart) action {
	var a action
	if fs.marker {
		if d.open {
			a |= actClose
			d.open = false
		}
		if d.inRow { // a row cut short by the redraw: its text is its last drawing
			d.endRow(d.drawn)
		}
		d.framed = true
		d.prev, d.cur = d.cur, d.prev[:0]
		if fs.counted && fs.up < len(d.prev) {
			d.prev = d.prev[:0] // a missed frame start: compare with nothing
		}
	}
	var dup bool
	if d.framed {
		pos := len(d.cur)
		dup = pos < len(d.prev) && d.prev[pos].same(key)
		if how == endCR && d.inRow && d.drawn.same(key) {
			dup = true
		}
	} else {
		dup = d.last.same(key)
	}
	switch {
	case !dup:
		a |= actLine
	case d.open && !d.shown.same(key):
		// A repeat, but the log's open row shows another drawing of this
		// row: write it, so the row the log shows last is the screen's.
		a |= actLine
	case d.open && how == endNL:
		a |= actNewline // the log shows this text already; finish the row
	}
	if a&(actLine|actNewline) != 0 {
		d.open = how == endCR && a&actLine != 0
		d.shown = key
	}
	d.last = key
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
