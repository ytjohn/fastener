package monitor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/parser"
	"github.com/ytjohn/fastener/internal/session"
)

// TestFinishWinsOverConcurrentUpdates hammers parser updates and flushes while
// the monitor finishes. The exited snapshot must be what ends up on disk, with
// every field it carried, and nothing may be written after it.
func TestFinishWinsOverConcurrentUpdates(t *testing.T) {
	m := newTestMonitor(t)
	p, ok := parser.Get("claude-stream-json")
	if !ok {
		t.Fatal("no claude-stream-json parser")
	}
	m.update(func(s *session.Status) { s.State = session.StateRunning })

	pr, pw := io.Pipe()
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		m.pump(pr, io.Discard, p)
	}()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the agent: an endless stream of changing results
		defer wg.Done()
		defer pw.Close()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			fmt.Fprintf(pw, `{"type":"result","subtype":"success","session_id":"s-%d","result":"r%d","is_error":%v,"total_cost_usd":%d,"usage":{"input_tokens":%d,"output_tokens":%d}}`+"\n",
				i, i, i%2 == 0, i, i, i)
		}
	}()
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() { // flushers racing the final write
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.flush()
			}
		}()
	}

	time.Sleep(30 * time.Millisecond)
	code := 7
	var want session.Status
	m.finish(session.EndedByExit, func(s *session.Status) {
		s.ExitCode = &code
		want = cloneStatus(*s)
	})
	first, err := os.ReadFile(m.d.StatusPath())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // keep hammering after the final write
	close(stop)
	wg.Wait()
	<-pumped

	last, err := os.ReadFile(m.d.StatusPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, last) {
		t.Fatalf("status.json changed after the exited write:\n%s\nthen\n%s", first, last)
	}
	var st session.Status
	if err := session.ReadJSON(m.d.StatusPath(), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != session.StateExited || st.EndedBy != session.EndedByExit || st.EndedAt == nil {
		t.Fatalf("state=%s ended_by=%s ended_at=%v", st.State, st.EndedBy, st.EndedAt)
	}
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("exit_code=%v", st.ExitCode)
	}
	if st.Monitor == nil || st.BootID == "" || st.Generation != 2 || st.SchemaVersion != session.SchemaVersion {
		t.Fatalf("identity missing: %+v", st)
	}
	if !strings.HasPrefix(want.ConversationID, "s-") {
		t.Fatalf("parser made no progress before finish: %+v", want.Fields)
	}
	if st.ConversationID != want.ConversationID || st.FinalMessage != want.FinalMessage ||
		st.Usage == nil || want.Usage == nil || st.Usage.CostUSD != want.Usage.CostUSD ||
		*st.Usage.InputTokens != *want.Usage.InputTokens || *st.AgentError != *want.AgentError {
		t.Fatalf("fields on disk %+v, want the finish snapshot %+v", st.Fields, want.Fields)
	}
}

// TestStaleSnapshotDropped checks the sequence rule directly: an older
// snapshot persisted after a newer one is ignored.
func TestStaleSnapshotDropped(t *testing.T) {
	m := newTestMonitor(t)
	m.mu.Lock()
	m.st.State = session.StateStarting
	old, oldSeq, _ := m.snapshotLocked()
	m.st.State = session.StateRunning
	cur, curSeq, _ := m.snapshotLocked()
	m.mu.Unlock()
	m.persist(cur, curSeq)
	m.persist(old, oldSeq)
	var st session.Status
	if err := session.ReadJSON(m.d.StatusPath(), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != session.StateRunning {
		t.Fatalf("stale snapshot overwrote a newer one: state=%s", st.State)
	}
}
