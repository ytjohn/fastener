package monitor

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	if err := os.WriteFile(envFile, []byte("B=file\nC=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		session.WriteJSON(d.SessionPath(), session.Session{
			SchemaVersion: 1, Name: "m", CreatedAt: time.Now(),
			EnvBase: []string{"A=base", "B=base"}, EnvFile: envFile, Env: []string{"C=flag"},
		}),
		session.WriteJSON(d.PlanPath(2), session.Plan{SchemaVersion: 1, Generation: 2, Argv: []string{"/bin/true"}, Marker: "m:2:n"}),
		session.WriteJSON(d.StatusPath(), session.Status{SchemaVersion: 1, State: session.StateStarting}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
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

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
	fakeDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

func buildFake(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeagent")
		if err != nil {
			fakeErr = err
			return
		}
		fakeDir = dir
		fakeBin = filepath.Join(dir, "fakeagent")
		if out, err := exec.Command("go", "build", "-o", fakeBin, "../testbin/fakeagent").CombinedOutput(); err != nil {
			fakeErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatalf("build fakeagent: %v", fakeErr)
	}
	return fakeBin
}

func setupRun(t *testing.T, argv []string, cwd string, env []string, maxRuntime time.Duration) session.Dir {
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
		Grace: session.Duration(300 * time.Millisecond), MaxRuntime: session.Duration(maxRuntime), Env: env, EnvBase: []string{"PATH=" + os.Getenv("PATH")},
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
	return runWith(t, d, make(chan os.Signal, 4), nil)
}

// runWith drives m.run with an injectable signal channel; inject (optional) runs
// concurrently once the monitor has started.
func runWith(t *testing.T, d session.Dir, sigs chan os.Signal, inject func()) session.Status {
	t.Helper()
	m, err := load(d, 1, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- m.run(sigs) }()
	if inject != nil {
		go inject()
	}
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
	d := setupRun(t, []string{bin}, t.TempDir(), nil, 0)
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
	d := setupRun(t, []string{bin}, "/nonexistent/fastener-cwd", nil, 0)
	st := runInProcess(t, d)
	if st.State != session.StateExited || st.EndedBy != session.EndedByLaunchFailed || st.Error == "" {
		t.Fatalf("state=%s ended_by=%s err=%q", st.State, st.EndedBy, st.Error)
	}
}

func requestStop(t *testing.T, d session.Dir) {
	t.Helper()
	if err := session.WriteJSON(d.StopPath(), session.StopRequest{SchemaVersion: 1, Generation: 1, By: "stop"}); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runningStatus(d session.Dir) bool {
	var st session.Status
	return session.ReadJSON(d.StatusPath(), &st) == nil && st.State == session.StateRunning
}

func TestStopIgnoringTermIsKilled(t *testing.T) {
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_IGNORE_TERM=1", "FAKE_SLEEP=30s"}, 0)
	sigs := make(chan os.Signal, 4)
	start := time.Now()
	st := runWith(t, d, sigs, func() {
		waitFor(t, "running", func() bool { return runningStatus(d) })
		time.Sleep(100 * time.Millisecond)
		requestStop(t, d)
		sigs <- syscall.SIGTERM
	})
	if st.EndedBy != session.EndedByStop || st.Signal != "SIGKILL" || st.ExitCode != nil {
		t.Fatalf("ended_by=%s signal=%s code=%v", st.EndedBy, st.Signal, st.ExitCode)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestMaxRuntimeLimit(t *testing.T) {
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_SLEEP=30s"}, 300*time.Millisecond)
	st := runInProcess(t, d)
	if st.EndedBy != session.EndedByLimit || st.Signal == "" {
		t.Fatalf("ended_by=%s signal=%s", st.EndedBy, st.Signal)
	}
}

func TestSignalWithoutRequest(t *testing.T) {
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_SLEEP=30s"}, 0)
	sigs := make(chan os.Signal, 4)
	st := runWith(t, d, sigs, func() {
		waitFor(t, "running", func() bool { return runningStatus(d) })
		sigs <- syscall.SIGTERM
	})
	if st.EndedBy != session.EndedBySignal {
		t.Fatalf("ended_by=%s", st.EndedBy)
	}
}

func TestStopBeforeSpawn(t *testing.T) {
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), nil, 0)
	requestStop(t, d)
	st := runInProcess(t, d)
	if st.EndedBy != session.EndedByStop || st.Agent != nil {
		t.Fatalf("ended_by=%s agent=%v", st.EndedBy, st.Agent)
	}
}

func TestRunUnknownParserIsLaunchFailed(t *testing.T) {
	bin := buildFake(t)
	d := setupRun(t, []string{bin}, t.TempDir(), nil, 0)
	var p session.Plan
	if err := session.ReadJSON(d.PlanPath(1), &p); err != nil {
		t.Fatal(err)
	}
	p.Parser = "nope"
	if err := session.WriteJSON(d.PlanPath(1), p); err != nil {
		t.Fatal(err)
	}
	st := runInProcess(t, d)
	if st.EndedBy != session.EndedByLaunchFailed || !strings.Contains(st.Error, `unknown parser "nope"`) || st.Agent != nil {
		t.Fatalf("ended_by=%s err=%q agent=%v", st.EndedBy, st.Error, st.Agent)
	}
}
