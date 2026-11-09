package monitor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/parser"
)

// recordingParser wraps a parser and records every line it is given.
type recordingParser struct {
	p     parser.Parser
	lines int
}

func (r *recordingParser) Line(line []byte, f *parser.Fields) bool {
	r.lines++
	return r.p.Line(line, f)
}

// bridgeFrame renders one redraw of a Claude-like status screen.
func bridgeFrame(env string, sessions []string, prev int) (string, int) {
	var b strings.Builder
	if prev > 0 {
		fmt.Fprintf(&b, "\x1b[%dA\x1b[J", prev)
	}
	lines := []string{"·✔︎· Connected · example · main", fmt.Sprintf("    Capacity: %d/32", len(sessions))}
	for _, s := range sessions {
		lines = append(lines, "    \x1b]8;;https://claude.ai/code/"+s+"?from=cli\x07Attached\x1b]8;;\x07")
	}
	lines = append(lines, "", "Continue in https://claude.ai/code?environment="+env, "space to show QR code")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String(), len(lines)
}

// TestPumpDedupeFrames: 10,000 redraws give a small output.log, while the
// parser still sees every line, including changes deep into the stream.
func TestPumpDedupeFrames(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	rp := &recordingParser{p: mustParser(t, "claude-remote")}
	var in bytes.Buffer
	prev, lines := 0, 0
	var sessions []string
	env := "env_A"
	for i := 0; i < 10000; i++ {
		switch i {
		case 5000:
			sessions = append(sessions, "session_S1")
		case 9000:
			env = "env_B"
		}
		var f string
		f, prev = bridgeFrame(env, sessions, prev)
		in.WriteString(f)
		lines += prev
	}
	raw := in.Len()
	var out bytes.Buffer
	m.pump(&in, &out, rp)
	t.Logf("%d bytes in, %d bytes in output.log", raw, out.Len())
	if out.Len() > 2048 {
		t.Errorf("output.log is %d bytes for %d frames (%d bytes raw); want it small", out.Len(), 10000, raw)
	}
	if rp.lines != lines {
		t.Errorf("parser saw %d lines, want %d", rp.lines, lines)
	}
	if m.st.RemoteURL != "https://claude.ai/code?environment=env_B" ||
		!slices.Equal(m.st.RemoteSessions, []string{"https://claude.ai/code/session_S1"}) {
		t.Errorf("fields = %+v", m.st.Fields)
	}
	// Changed lines are in the log with their raw bytes.
	for _, want := range []string{
		"Capacity: 1/32", "\x1b]8;;https://claude.ai/code/session_S1?from=cli\x07Attached",
		"environment=env_A\n", "environment=env_B\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output.log lacks %q", want)
		}
	}
}

// TestPumpDedupeKeepsDistinctLines: without repeats, dedupe changes nothing
// but leading cursor escapes; an unterminated last line is written too.
func TestPumpDedupeKeepsDistinctLines(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	var in strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&in, "\x1b[1mline %d\x1b[0m\n", i)
	}
	in.WriteString("tail without newline")
	var out bytes.Buffer
	m.pump(strings.NewReader(in.String()), &out, mustParser(t, "none"))
	if out.String() != in.String() {
		t.Errorf("output.log differs from the input (%d vs %d bytes)", out.Len(), in.Len())
	}
}

// TestPumpDedupeStateChangesLogged: a screen that changes and changes back
// logs every state, because a line is compared with the same position in the
// previous frame only.
func TestPumpDedupeStateChangesLogged(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	var in strings.Builder
	prev := 0
	frame := func(state string) {
		if prev > 0 {
			fmt.Fprintf(&in, "\x1b[%dA\x1b[J", prev)
		}
		in.WriteString("·✔︎· " + state + " · example · main\n    Capacity: 0/32\n\nspace to show QR code\n")
		prev = 4
	}
	for _, st := range []string{"Connected", "Connected", "Disconnected", "Disconnected", "Connected", "Connected"} {
		frame(st)
	}
	var out bytes.Buffer
	m.pump(strings.NewReader(in.String()), &out, mustParser(t, "claude-remote"))
	got := out.String()
	if n := strings.Count(got, " Connected "); n != 2 {
		t.Errorf("Connected logged %d times, want 2 (before and after the disconnect):\n%q", n, got)
	}
	if n := strings.Count(got, " Disconnected "); n != 1 {
		t.Errorf("Disconnected logged %d times, want 1:\n%q", n, got)
	}
	if n := strings.Count(got, "Capacity"); n != 1 {
		t.Errorf("an unchanged line was logged %d times, want 1:\n%q", n, got)
	}
	// Leading cursor movement and erase sequences are not written.
	if strings.Contains(got, "\x1b[4A") || strings.Contains(got, "\x1b[J") {
		t.Errorf("cursor escapes written: %q", got)
	}
}

// TestPumpDedupeNoFrameMarker: without a frame marker, only a line identical
// to the line right before it is a repeat.
func TestPumpDedupeNoFrameMarker(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	var out bytes.Buffer
	m.pump(strings.NewReader("a\na\na\nb\na\na\n"), &out, mustParser(t, "none"))
	if got := out.String(); got != "a\nb\na\n" {
		t.Errorf("output.log = %q, want %q", got, "a\nb\na\n")
	}
}

// TestPumpDedupeLongLinesAlwaysWritten: lines over dedupeMaxLine are never
// de-duplicated.
func TestPumpDedupeLongLinesAlwaysWritten(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	long := strings.Repeat("y", dedupeMaxLine+1) + "\n"
	var out bytes.Buffer
	m.pump(strings.NewReader(long+long+long), &out, mustParser(t, "none"))
	if out.Len() != 3*len(long) {
		t.Errorf("output.log is %d bytes, want %d", out.Len(), 3*len(long))
	}
}

// TestPumpDedupeCarriageReturn: 20 MB of "\r"-only redraws of one line are
// parsed and de-duplicated as lines, not held as one 20 MB line.
func TestPumpDedupeCarriageReturn(t *testing.T) {
	const total = 20 << 20
	const redraw = "\x1b[2K Connected · https://claude.ai/code?environment=env_cr_01\r"
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	rp := &recordingParser{p: mustParser(t, "claude-remote")}
	n := total / len(redraw)
	in := io.MultiReader(&repeatReader{s: []byte(redraw), n: n}, strings.NewReader("\r\nbye\n"))
	var out bytes.Buffer
	peak := heapPeak(func() { m.pump(in, &out, rp) })
	t.Logf("peak heap growth %d KB for %d MB of redraws", peak>>10, total>>20)
	// Holding the redraws as one line would take at least 16 MB.
	if peak > 12<<20 {
		t.Errorf("heap grew by %d MB while pumping %d MB of redraws; memory is not bounded", peak>>20, total>>20)
	}
	if m.st.RemoteURL != "https://claude.ai/code?environment=env_cr_01" || m.st.ParseErrors != 0 {
		t.Errorf("fields = %+v", m.st.Fields)
	}
	if rp.lines < n {
		t.Errorf("parser saw %d lines, want at least %d", rp.lines, n)
	}
	if out.Len() > 1024 {
		t.Errorf("output.log is %d bytes for %d redraws; want it small", out.Len(), n)
	}
	if !strings.HasSuffix(out.String(), "bye\n") {
		t.Errorf("output.log tail = %q", out.String())
	}
}

// repeatReader yields s n times.
type repeatReader struct {
	s   []byte
	n   int
	off int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	w := 0
	for w < len(p) && r.n > 0 {
		c := copy(p[w:], r.s[r.off:])
		w += c
		r.off += c
		if r.off == len(r.s) {
			r.off = 0
			r.n--
		}
	}
	if w == 0 {
		return 0, io.EOF
	}
	return w, nil
}

// TestPumpDedupeOverlongLine: an overlong line in dedupe mode is written in
// full, with bounded memory, and the next line is still parsed.
func TestPumpDedupeOverlongLine(t *testing.T) {
	const big = 64 << 20
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	p := mustParser(t, "claude-remote")
	outPath := filepath.Join(t.TempDir(), "output.log")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	const after = "https://claude.ai/code?environment=env_after\n"
	in := io.MultiReader(strings.NewReader("head\n"), &fillReader{b: 'x', n: big}, strings.NewReader("\nhead\n"+after))
	runtime.GC()
	var before, end runtime.MemStats
	runtime.ReadMemStats(&before)
	m.pump(in, out, p)
	runtime.ReadMemStats(&end)
	if alloc := end.TotalAlloc - before.TotalAlloc; alloc > big*3/4 {
		t.Errorf("pump allocated %d MB for a %d MB line; memory is not bounded", alloc>>20, big>>20)
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	// Everything is written: the second "head" follows the overlong line, not
	// the first "head".
	if want := int64(2*len("head\n") + big + 1 + len(after)); fi.Size() != want {
		t.Errorf("output.log is %d bytes, want %d", fi.Size(), want)
	}
	if m.st.ParseErrors != 1 || m.st.RemoteURL != "https://claude.ai/code?environment=env_after" {
		t.Errorf("fields = %+v", m.st.Fields)
	}
}

func mustParser(t *testing.T, name string) parser.Parser {
	t.Helper()
	p, ok := parser.Get(name)
	if !ok {
		t.Fatalf("no parser %s", name)
	}
	return p
}

// heapPeak runs f and returns the largest growth of the live heap over its
// value before f, sampled every few milliseconds.
func heapPeak(f func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	stop := make(chan struct{})
	res := make(chan uint64)
	go func() {
		var peak uint64
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > base && ms.HeapAlloc-base > peak {
				peak = ms.HeapAlloc - base
			}
			select {
			case <-stop:
				res <- peak
				return
			case <-t.C:
			}
		}
	}()
	f()
	close(stop)
	return <-res
}

// TestPumpDedupeRealCapture runs the sanitized Claude capture through the
// pump: every distinct state is logged once, without cursor escapes.
func TestPumpDedupeRealCapture(t *testing.T) {
	raw, err := os.ReadFile("../parser/testdata/claude-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := newTestMonitor(t)
	m.plan.DedupeFrames = true
	var out bytes.Buffer
	m.pump(bytes.NewReader(raw), &out, mustParser(t, "claude-remote"))
	got := out.String()
	t.Logf("%d bytes in, %d out:\n%s", len(raw), out.Len(), got)
	for _, want := range []string{"Connecting", "Capacity: 0/32", "Capacity: 1/32", "Attached", "example-session", "Environment preserved"} {
		if !strings.Contains(got, want) {
			t.Errorf("output.log lacks %q", want)
		}
	}
	if strings.Contains(got, "\x1b[J") || out.Len() > len(raw)/2 {
		t.Errorf("cursor escapes kept or too little dropped (%d of %d bytes)", out.Len(), len(raw))
	}
	if m.st.RemoteURL == "" || len(m.st.RemoteSessions) != 1 {
		t.Errorf("fields = %+v", m.st.Fields)
	}
}
