package monitor

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ytjohn/fastener/internal/parser"
)

// fillReader yields n copies of b without allocating them.
type fillReader struct {
	b byte
	n int64
}

func (f *fillReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > f.n {
		p = p[:f.n]
	}
	for i := range p {
		p[i] = f.b
	}
	f.n -= int64(len(p))
	return len(p), nil
}

const (
	initLine   = `{"type":"system","subtype":"init","session_id":"big-0001"}` + "\n"
	resultLine = `{"type":"result","subtype":"success","session_id":"big-0001","result":"after","is_error":false}` + "\n"
)

// TestPumpOverlongLineBoundedMemory pushes a 64 MB single line through pump.
// output.log must get every byte, the line counts as one parse error, the
// next line is still parsed, and the pump must not hold the whole line.
func TestPumpOverlongLineBoundedMemory(t *testing.T) {
	const big = 64 << 20
	m := newTestMonitor(t)
	p, _ := parser.Get("claude-stream-json")
	outPath := filepath.Join(t.TempDir(), "output.log")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in := io.MultiReader(
		bytes.NewReader([]byte(initLine)),
		&fillReader{b: 'x', n: big},
		bytes.NewReader([]byte("\n"+resultLine)),
	)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m.pump(in, out, p)
	runtime.ReadMemStats(&after)

	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("pump allocated %d MB for a %d MB line", alloc>>20, big>>20)
	if alloc > big*3/4 {
		t.Errorf("pump allocated %d MB for a %d MB line; memory is not bounded", alloc>>20, big>>20)
	}
	m.mu.Lock()
	f := m.st.Fields
	m.mu.Unlock()
	if f.ParseErrors != 1 {
		t.Errorf("parse_errors = %d, want 1", f.ParseErrors)
	}
	if f.ConversationID != "big-0001" || f.FinalMessage != "after" {
		t.Errorf("the line after the overlong one was not parsed: %+v", f)
	}

	// output.log holds every byte, unchanged.
	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(initLine) + big + 1 + len(resultLine)); fi.Size() != want {
		t.Fatalf("output.log is %d bytes, want %d", fi.Size(), want)
	}
	rf, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	br := bufio.NewReader(rf)
	head := make([]byte, len(initLine))
	if _, err := io.ReadFull(br, head); err != nil || string(head) != initLine {
		t.Fatalf("output.log head = %q (%v)", head, err)
	}
	buf := make([]byte, 1<<20)
	for left := big; left > 0; {
		n, err := br.Read(buf[:min(len(buf), left)])
		for _, c := range buf[:n] {
			if c != 'x' {
				t.Fatalf("output.log: unexpected byte %q inside the long line", c)
			}
		}
		left -= n
		if err != nil {
			t.Fatalf("output.log ended early: %v", err)
		}
	}
	tail, _ := io.ReadAll(br)
	if string(tail) != "\n"+resultLine {
		t.Fatalf("output.log tail = %q", tail)
	}
}

// TestPumpLineAtCap: a line just under the cap is parsed; one just over is not.
func TestPumpLineAtCap(t *testing.T) {
	m := newTestMonitor(t)
	p, _ := parser.Get("claude-stream-json")
	// A valid JSON line padded with spaces to exactly maxParseLine bytes incl. "\n".
	body := `{"type":"system","subtype":"init","session_id":"cap-ok"}`
	fits := body + string(bytes.Repeat([]byte(" "), maxParseLine-len(body)-1)) + "\n"
	over := `{"type":"system","subtype":"init","session_id":"cap-over"}` + string(bytes.Repeat([]byte(" "), maxParseLine)) + "\n"
	var sink bytes.Buffer
	m.pump(bytes.NewReader([]byte(fits+over)), &sink, p)
	if sink.Len() != len(fits)+len(over) {
		t.Fatalf("wrote %d bytes, want %d", sink.Len(), len(fits)+len(over))
	}
	if m.st.ConversationID != "cap-ok" || m.st.ParseErrors != 1 {
		t.Fatalf("conversation_id=%q parse_errors=%d", m.st.ConversationID, m.st.ParseErrors)
	}
}

// TestPumpUnterminatedLastLine: a final line without a newline is still parsed.
func TestPumpUnterminatedLastLine(t *testing.T) {
	m := newTestMonitor(t)
	p, _ := parser.Get("claude-stream-json")
	var sink bytes.Buffer
	m.pump(bytes.NewReader([]byte(initLine+`{"type":"result","subtype":"success","session_id":"big-0001","result":"tail","is_error":false}`)), &sink, p)
	if m.st.FinalMessage != "tail" || m.st.ParseErrors != 0 {
		t.Fatalf("final_message=%q parse_errors=%d", m.st.FinalMessage, m.st.ParseErrors)
	}
}
