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

// TestPumpDedupeKeepsDistinctLines: without repeats, dedupe changes nothing;
// an unterminated last line is written too.
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
	// A repeat outside the window is written again.
	m2 := newTestMonitor(t)
	m2.plan.DedupeFrames = true
	var in2 strings.Builder
	in2.WriteString("first\n")
	for i := 0; i < dedupeWindow; i++ {
		fmt.Fprintf(&in2, "filler %d\n", i)
	}
	in2.WriteString("first\nfirst\n")
	out.Reset()
	m2.pump(strings.NewReader(in2.String()), &out, mustParser(t, "none"))
	if n := strings.Count(out.String(), "first\n"); n != 2 {
		t.Errorf("first written %d times, want 2 (once, then again after leaving the window)", n)
	}
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
	// "head" again is a repeat and is dropped; everything else is written.
	if want := int64(len("head\n") + big + 1 + len(after)); fi.Size() != want {
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
