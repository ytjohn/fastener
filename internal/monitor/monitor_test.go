package monitor

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
)

func newTestMonitor(t *testing.T) *monitor {
	t.Helper()
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	d, err := session.Create("m")
	if err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envFile, []byte("B=file\nC=file\n"), 0o600)
	session.WriteJSON(d.SessionPath(), session.Session{
		SchemaVersion: 1, Name: "m", CreatedAt: time.Now(),
		EnvBase: []string{"A=base", "B=base"}, EnvFile: envFile, Env: []string{"C=flag"},
	})
	session.WriteJSON(d.PlanPath(2), session.Plan{SchemaVersion: 1, Generation: 2, Argv: []string{"/bin/true"}, Marker: "m:2:n"})
	session.WriteJSON(d.StatusPath(), session.Status{SchemaVersion: 1, State: session.StateStarting})
	m, err := load(d, 2, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStopRequestScopedToGeneration(t *testing.T) {
	m := newTestMonitor(t)
	session.WriteJSON(m.d.StopPath(), session.StopRequest{SchemaVersion: 1, Generation: 1, By: "stop"})
	if _, ok := m.stopRequest(); ok {
		t.Fatal("stale request from generation 1 honoured by generation 2")
	}
	session.WriteJSON(m.d.StopPath(), session.StopRequest{SchemaVersion: 1, Generation: 2, By: "stop"})
	if _, ok := m.stopRequest(); !ok {
		t.Fatal("request for this generation ignored")
	}
}

func TestAgentEnvLayering(t *testing.T) {
	m := newTestMonitor(t)
	env, err := m.agentEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "base", "B": "file", "C": "flag", proc.MarkerEnv: "m:2:n"}
	got := map[string]string{}
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				got[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (env %v)", k, got[k], v, env)
		}
	}
}

func buildFake(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakeagent")
	if out, err := exec.Command("go", "build", "-o", bin, "../testbin/fakeagent").CombinedOutput(); err != nil {
		t.Fatalf("build fakeagent: %v\n%s", err, out)
	}
	return bin
}

func setupRun(t *testing.T, argv []string, cwd string) session.Dir {
	t.Helper()
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	d, err := session.Create("e2e")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(session.WriteJSON(d.SessionPath(), session.Session{
		SchemaVersion: 1, Name: "e2e", CreatedAt: time.Now(), Cwd: cwd,
		Grace: session.Duration(2 * time.Second), EnvBase: []string{"PATH=" + os.Getenv("PATH")},
	}))
	must(os.WriteFile(d.PromptPath(), []byte("ping\n"), 0o600))
	must(session.WriteJSON(d.PlanPath(1), session.Plan{
		SchemaVersion: 1, Generation: 1, Argv: argv, PromptMode: "stdin",
		Parser: "claude-stream-json", Marker: "e2e:1:n",
	}))
	must(session.WriteJSON(d.StatusPath(), session.Status{SchemaVersion: 1, State: session.StateStarting}))
	return d
}

func runInProcess(t *testing.T, d session.Dir) session.Status {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- Run(string(d), 1, log.New(os.Stderr, "", 0)) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("Run = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish")
	}
	var st session.Status
	if err := session.ReadJSON(d.StatusPath(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRunEndToEnd(t *testing.T) {
	bin := buildFake(t)
	d := setupRun(t, []string{bin}, t.TempDir())
	st := runInProcess(t, d)
	if st.State != session.StateExited || st.EndedBy != session.EndedByExit {
		t.Fatalf("state=%s ended_by=%s err=%s", st.State, st.EndedBy, st.Error)
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exit code = %v", st.ExitCode)
	}
	if st.ConversationID != "fake-session-0001" || st.FinalMessage != "pong" {
		t.Fatalf("fields: %+v", st.Fields)
	}
	if st.Monitor == nil || st.Agent == nil || st.BootID == "" || st.Generation != 1 {
		t.Fatalf("identity missing: %+v", st)
	}
	out, _ := os.ReadFile(d.OutputLog())
	if !strings.Contains(string(out), `"subtype":"init"`) || !strings.Contains(string(out), `"result":"pong"`) {
		t.Fatalf("output.log: %s", out)
	}
}

func TestRunLaunchFailed(t *testing.T) {
	bin := buildFake(t)
	d := setupRun(t, []string{bin}, "/nonexistent/fastener-cwd")
	st := runInProcess(t, d)
	if st.State != session.StateExited || st.EndedBy != session.EndedByLaunchFailed || st.Error == "" {
		t.Fatalf("state=%s ended_by=%s err=%q", st.State, st.EndedBy, st.Error)
	}
}
