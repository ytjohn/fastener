package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStateFixtures loads every session in every testdata/state/<release>
// directory with the current code. Design §4 Upgrades: a newer CLI must read
// the state an older release wrote.
func TestStateFixtures(t *testing.T) {
	rels, err := filepath.Glob("testdata/state/v*")
	if err != nil || len(rels) == 0 {
		t.Fatalf("no fixture releases found: %v", err)
	}
	for _, rel := range rels {
		abs, err := filepath.Abs(rel)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(rel), func(t *testing.T) {
			t.Setenv("FASTENER_STATE_DIR", abs)
			names, err := Names()
			if err != nil || len(names) == 0 {
				t.Fatalf("Names: %v %v", names, err)
			}
			for _, n := range names {
				for _, load := range []struct {
					how string
					fn  func(string, time.Duration) (*View, error)
				}{{"Load", Load}, {"LoadFull", LoadFull}} {
					v, err := load.fn(n, time.Second)
					if err != nil {
						t.Fatalf("%s %s: %v", load.how, n, err)
					}
					if v.State != StateExited {
						t.Errorf("%s %s: state %q, want exited", load.how, n, v.State)
					}
					if v.Session.Name != n || v.Session.Backend != "fake" || v.Session.Kind != "headless" {
						t.Errorf("%s %s: session %+v", load.how, n, v.Session)
					}
					if v.Plan == nil || v.Plan.Generation != 1 || v.Plan.Marker == "" || v.Plan.Parser != "claude-stream-json" {
						t.Errorf("%s %s: plan %+v", load.how, n, v.Plan)
					}
					if v.Launcher == nil || v.Launcher.Launcher != "setsid" || v.Launcher.Monitor == nil {
						t.Errorf("%s %s: launcher %+v", load.how, n, v.Launcher)
					}
				}
			}
		})
	}
}

func TestStateFixtureV010Fields(t *testing.T) {
	abs, _ := filepath.Abs("testdata/state/v0.1.0")
	t.Setenv("FASTENER_STATE_DIR", abs)
	get := func(n string) *View {
		t.Helper()
		v, err := LoadFull(n, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	ok := get("ok")
	s := ok.Status
	if s.EndedBy != EndedByExit || s.ExitCode == nil || *s.ExitCode != 0 || s.ConversationID != "fake-session-0001" ||
		s.FinalMessage != "pong" || s.Model != "fake-model-1" || s.Usage == nil || s.Usage.OutputTokens == nil || *s.Usage.OutputTokens != 1 ||
		s.AgentError == nil || *s.AgentError || s.StartedAt == nil || s.EndedAt == nil || s.Agent == nil {
		t.Errorf("ok status: %+v", s)
	}
	if ok.Session.Grace != Duration(10*time.Second) || len(ok.Session.Env) != 2 {
		t.Errorf("ok session: %+v", ok.Session)
	}
	if b, err := os.ReadFile(ok.Dir.PromptPath()); err != nil || string(b) != "hello" {
		t.Errorf("prompt: %q %v", b, err)
	}

	bad := get("bad")
	if bad.Status.EndedBy != EndedByLaunchFailed || bad.Status.Error == "" || bad.Status.Agent != nil || bad.Status.ExitCode != nil {
		t.Errorf("bad status: %+v", bad.Status)
	}

	st := get("stopped")
	if st.Status.EndedBy != EndedByStop || st.Status.Signal != "SIGKILL" || st.Status.ExitCode != nil {
		t.Errorf("stopped status: %+v", st.Status)
	}
	var req StopRequest
	if err := ReadJSON(st.Dir.StopPath(), &req); err != nil || req.By != "stop" || req.Grace != Duration(time.Second) {
		t.Errorf("stop request: %+v %v", req, err)
	}
}
