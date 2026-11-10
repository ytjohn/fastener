package monitor

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/attach"
	"github.com/ytjohn/fastener/internal/session"
)

// ptyRun is a monitor running an interactive (pty) generation of the fake
// agent in-process, driven by the test.
type ptyRun struct {
	t    *testing.T
	d    session.Dir
	sigs chan os.Signal
	done chan int
}

// startPTY runs the fake agent in pty mode (plus env), with the plan
// adjusted by mutate. Cleanup stops and kills whatever is left.
func startPTY(t *testing.T, env []string, mutate func(*session.Plan)) *ptyRun {
	t.Helper()
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), append([]string{"FAKE_PTY=1"}, env...), 0)
	var p session.Plan
	if err := session.ReadJSON(d.PlanPath(1), &p); err != nil {
		t.Fatal(err)
	}
	p.PTY, p.PromptMode, p.Parser = &session.PTYPlan{}, "none", "none"
	if mutate != nil {
		mutate(&p)
	}
	if err := session.WriteJSON(d.PlanPath(1), p); err != nil {
		t.Fatal(err)
	}
	m, err := load(d, 1, log.New(os.Stderr, "monitor: ", 0))
	if err != nil {
		t.Fatal(err)
	}
	r := &ptyRun{t: t, d: d, sigs: make(chan os.Signal, 4), done: make(chan int, 1)}
	go func() { r.done <- m.run(r.sigs) }()
	t.Cleanup(func() {
		select {
		case <-r.done:
			r.done <- 0
			return
		default:
		}
		requestStop(t, d)
		r.sigs <- syscall.SIGTERM
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			if st := r.status(); st.PGID > 0 {
				_ = syscall.Kill(-st.PGID, syscall.SIGKILL)
			}
		}
	})
	return r
}

func (r *ptyRun) status() session.Status {
	var st session.Status
	_ = session.ReadJSON(r.d.StatusPath(), &st)
	return st
}

// running waits for the running record and returns it.
func (r *ptyRun) running() session.Status {
	r.t.Helper()
	waitFor(r.t, "running", func() bool { return runningStatus(r.d) })
	st := r.status()
	if st.AttachSocket == "" || st.AttachedClients == nil {
		r.t.Fatalf("running pty session without attach fields: %+v", st)
	}
	return st
}

// wait waits for the monitor to finish and returns the final status.
func (r *ptyRun) wait() session.Status {
	r.t.Helper()
	select {
	case code := <-r.done:
		r.done <- code // for Cleanup
		if code != 0 {
			r.t.Fatalf("Run = %d", code)
		}
	case <-time.After(15 * time.Second):
		r.t.Fatal("monitor did not finish")
	}
	return r.status()
}

func (r *ptyRun) attach(size attach.Size) *attach.Client {
	r.t.Helper()
	c, err := attach.Dial(r.status().AttachSocket, attach.Options{Size: size})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { c.Close() })
	return c
}

// recv reads output until it contains want, keeping only a bounded tail so
// a flood stays cheap. It returns the exit message if one arrives first and
// want is "".
func recv(t *testing.T, c *attach.Client, want string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	var tail []byte
	for {
		m, err := c.Recv()
		if err != nil {
			t.Fatalf("waiting for %q (last output %q): %v", want, tail, err)
		}
		tail = append(tail, m.Data...)
		if bytes.Contains(tail, []byte(want)) {
			return
		}
		if keep := len(want) + 4096; len(tail) > keep {
			tail = append(tail[:0], tail[len(tail)-keep:]...)
		}
	}
}

// recvExit reads until the exit message.
func recvExit(t *testing.T, c *attach.Client) *attach.ExitInfo {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		m, err := c.Recv()
		if err != nil {
			t.Fatalf("waiting for exit: %v", err)
		}
		if m.Exit != nil {
			return m.Exit
		}
	}
}

func socketGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attach socket %s still there after exit: %v", path, err)
	}
}

// TestPTYAgentSeesTTYNobodyAttached: the agent runs on a terminal of the
// plan's size with it as controlling terminal; with nobody attached its
// output is still drained into output.log and its exit code recorded.
func TestPTYAgentSeesTTYNobodyAttached(t *testing.T) {
	const flood = 300_000 // far more than a pty's buffer
	r := startPTY(t, []string{"FAKE_PTY_AUTOEXIT=1", "FAKE_EXIT=3", "FAKE_PTY_FLOOD=300000"},
		func(p *session.Plan) { p.PTY = &session.PTYPlan{Rows: 30, Cols: 100} })
	st := r.wait()
	if st.EndedBy != session.EndedByExit || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("ended_by=%s exit=%v err=%s", st.EndedBy, st.ExitCode, st.Error)
	}
	if st.AttachSocket != "" || st.AttachedClients != nil {
		t.Fatalf("attach fields left in the final status: %+v", st)
	}
	out, _ := os.ReadFile(r.d.OutputLog())
	if !bytes.Contains(out, []byte("tty=true ctty=true size=30x100")) {
		t.Fatalf("agent did not see a controlling tty of 30x100:\n%.300s", out)
	}
	if n := bytes.Count(out, []byte("x")); n < flood-flood/100 || !bytes.Contains(out, []byte("flood-done")) {
		t.Fatalf("output.log has %d x and flood-done=%v", n, bytes.Contains(out, []byte("flood-done")))
	}
	p, _ := attach.SocketPath(string(r.d))
	socketGone(t, p)
}

func TestPTYDefaultSize(t *testing.T) {
	r := startPTY(t, []string{"FAKE_PTY_AUTOEXIT=1"}, nil)
	r.wait()
	out, _ := os.ReadFile(r.d.OutputLog())
	if !bytes.Contains(out, []byte("size=24x80")) {
		t.Fatalf("output.log: %s", out)
	}
}

// TestPTYAttachSession covers one client's life: attach resizes and
// SIGWINCHes, input is echoed, a resize reaches the agent, detaching leaves
// the agent running, and a later client sees the exit.
func TestPTYAttachSession(t *testing.T) {
	r := startPTY(t, nil, nil)
	st := r.running()
	c := r.attach(attach.Size{Rows: 40, Cols: 120})
	if c.Welcome.Version != attach.Version || c.Welcome.Size != (attach.Size{Rows: 40, Cols: 120}) {
		t.Fatalf("welcome %+v", c.Welcome)
	}
	recv(t, c, "winch=40x120")
	if err := c.Send([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	recv(t, c, "echo:hello")
	if err := c.Resize(attach.Size{Rows: 50, Cols: 150}); err != nil {
		t.Fatal(err)
	}
	recv(t, c, "winch=50x150")
	waitFor(t, "attached_clients=1", func() bool {
		s := r.status()
		return s.AttachedClients != nil && *s.AttachedClients == 1
	})

	if err := c.Detach(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attached_clients=0", func() bool {
		s := r.status()
		return s.AttachedClients != nil && *s.AttachedClients == 0
	})
	if s := r.status(); s.State != session.StateRunning {
		t.Fatalf("detach ended the session: %+v", s)
	}

	c2 := r.attach(attach.Size{})
	if c2.Welcome.Size != (attach.Size{Rows: 50, Cols: 150}) {
		t.Fatalf("second welcome %+v", c2.Welcome)
	}
	if err := c2.Send([]byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}
	exit := recvExit(t, c2)
	if exit.EndedBy != session.EndedByExit || exit.ExitCode == nil || *exit.ExitCode != 7 {
		t.Fatalf("exit frame %+v", exit)
	}
	fin := r.wait()
	if fin.ExitCode == nil || *fin.ExitCode != 7 {
		t.Fatalf("final status %+v", fin)
	}
	socketGone(t, st.AttachSocket)
	out, _ := os.ReadFile(r.d.OutputLog())
	if !bytes.Contains(out, []byte("echo:hello")) || !bytes.Contains(out, []byte("winch=50x150")) {
		t.Fatalf("output.log misses the attached session's output:\n%s", out)
	}
}

// TestPTYAttachSameSizeRedraws: attaching at the size the pty already has
// changes nothing for the kernel, so the monitor sends SIGWINCH itself.
func TestPTYAttachSameSizeRedraws(t *testing.T) {
	r := startPTY(t, nil, nil)
	r.running()
	c := r.attach(attach.Size{Rows: session.DefaultPTYRows, Cols: session.DefaultPTYCols})
	recv(t, c, "winch=24x80")
}

func TestPTYTwoClientsBothGetOutput(t *testing.T) {
	r := startPTY(t, nil, nil)
	r.running()
	a := r.attach(attach.Size{})
	b := r.attach(attach.Size{})
	if err := a.Send([]byte("from-a\n")); err != nil {
		t.Fatal(err)
	}
	recv(t, a, "echo:from-a")
	recv(t, b, "echo:from-a")
	if err := b.Send([]byte("from-b\n")); err != nil {
		t.Fatal(err)
	}
	recv(t, a, "echo:from-b")
	recv(t, b, "echo:from-b")
}

// TestPTYSlowClientDoesNotStall: a client that never reads is dropped once
// it is too far behind; the agent's output keeps flowing to output.log and
// to a client that reads.
func TestPTYSlowClientDoesNotStall(t *testing.T) {
	r := startPTY(t, nil, nil)
	r.running()
	slow := r.attach(attach.Size{})
	_ = slow
	fast := r.attach(attach.Size{})
	if err := fast.Send([]byte("flood 4000000\n")); err != nil {
		t.Fatal(err)
	}
	recv(t, fast, "flood-done")
	waitFor(t, "slow client dropped", func() bool {
		s := r.status()
		return s.AttachedClients != nil && *s.AttachedClients == 1
	})
	if err := fast.Send([]byte("exit 0\n")); err != nil {
		t.Fatal(err)
	}
	recvExit(t, fast)
	r.wait()
	out, _ := os.ReadFile(r.d.OutputLog())
	if !bytes.Contains(out, []byte("flood-done")) {
		t.Fatal("flood-done missing from output.log")
	}
}

// TestPTYStop: stop works on a pty session and the attached client is told.
func TestPTYStop(t *testing.T) {
	r := startPTY(t, nil, nil)
	st := r.running()
	c := r.attach(attach.Size{})
	requestStop(t, r.d)
	r.sigs <- syscall.SIGTERM
	exit := recvExit(t, c)
	if exit.EndedBy != session.EndedByStop {
		t.Fatalf("exit frame %+v", exit)
	}
	fin := r.wait()
	if fin.EndedBy != session.EndedByStop {
		t.Fatalf("ended_by=%s", fin.EndedBy)
	}
	socketGone(t, st.AttachSocket)
}

// TestPTYStopIgnoringTerm: an agent that ignores SIGTERM on its pty is
// killed after the grace, as headless ones are.
func TestPTYStopIgnoringTerm(t *testing.T) {
	r := startPTY(t, []string{"FAKE_IGNORE_TERM=1"}, nil)
	r.running()
	requestStop(t, r.d)
	r.sigs <- syscall.SIGTERM
	fin := r.wait()
	if fin.EndedBy != session.EndedByStop || fin.Signal != "SIGKILL" {
		t.Fatalf("ended_by=%s signal=%s", fin.EndedBy, fin.Signal)
	}
}

func TestPTYWrongVersionRejected(t *testing.T) {
	r := startPTY(t, nil, nil)
	st := r.running()
	_, err := attach.Dial(st.AttachSocket, attach.Options{Version: attach.Version + 1})
	var re *attach.RemoteError
	if !errors.As(err, &re) || re.Code != attach.CodeVersion {
		t.Fatalf("Dial with a newer version: %v", err)
	}
	// The session is unaffected.
	c := r.attach(attach.Size{})
	c.Send([]byte("still\n"))
	recv(t, c, "echo:still")
}

// TestPTYParserRunsOnTerminalOutput: the plan's parser reads the pty stream
// line by line, as it reads a headless agent's stdout.
func TestPTYParserRunsOnTerminalOutput(t *testing.T) {
	r := startPTY(t, []string{"FAKE_PTY_INIT=1", "FAKE_PTY_AUTOEXIT=1"},
		func(p *session.Plan) { p.Parser = "claude-stream-json" })
	st := r.wait()
	if st.ConversationID != "fake-pty-0001" {
		t.Fatalf("conversation_id=%q parse_errors=%d", st.ConversationID, st.ParseErrors)
	}
}

func TestPTYPromptOnStdinIsLaunchFailed(t *testing.T) {
	r := startPTY(t, nil, func(p *session.Plan) { p.PromptMode = "stdin" })
	st := r.wait()
	if st.EndedBy != session.EndedByLaunchFailed || !strings.Contains(st.Error, "pty") || st.Agent != nil {
		t.Fatalf("ended_by=%s err=%q", st.EndedBy, st.Error)
	}
}

// TestHeadlessPlanHasNoAttach: a plan without pty (every plan before v0.4)
// runs as before: no socket, no attach fields.
func TestHeadlessPlanHasNoAttach(t *testing.T) {
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_SLEEP=300ms"}, 0)
	sigs := make(chan os.Signal, 4)
	var during session.Status
	st := runWith(t, d, sigs, func() {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !runningStatus(d); time.Sleep(10 * time.Millisecond) {
		}
		_ = session.ReadJSON(d.StatusPath(), &during)
	})
	if st.EndedBy != session.EndedByExit || st.FinalMessage != "pong" {
		t.Fatalf("headless run: %+v", st)
	}
	if during.AttachSocket != "" || during.AttachedClients != nil {
		t.Fatalf("headless run has attach fields: %+v", during)
	}
	p, _ := attach.SocketPath(string(d))
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("headless run created %s", p)
	}
	b, _ := os.ReadFile(d.StatusPath())
	if bytes.Contains(b, []byte("attach")) {
		t.Fatalf("status.json mentions attach: %s", b)
	}
}

// TestFinishClearsAttachFields: every path to an outcome drops the attach
// fields, including those that never reach the spawn (a stop or a launch
// failure while a client was attached during starting).
func TestFinishClearsAttachFields(t *testing.T) {
	m := newTestMonitor(t)
	one := 1
	m.update(func(s *session.Status) { s.AttachSocket, s.AttachedClients = "/x/attach.sock", &one })
	m.launchFailed(errors.New("boom"))
	var st session.Status
	if err := session.ReadJSON(m.d.StatusPath(), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != session.StateExited || st.AttachSocket != "" || st.AttachedClients != nil {
		t.Fatalf("exit record keeps attach fields: %+v", st)
	}
}

// TestClientCountOnlyWhileRunning: a count reported while starting or after
// the outcome is not recorded.
func TestClientCountOnlyWhileRunning(t *testing.T) {
	m := newTestMonitor(t)
	p := &ptyIO{m: m}
	m.st.State = session.StateStarting
	p.clients(2)
	if m.st.AttachedClients != nil {
		t.Fatal("count recorded while starting")
	}
	m.st.State = session.StateRunning
	p.clients(3)
	if m.st.AttachedClients == nil || *m.st.AttachedClients != 3 {
		t.Fatalf("count while running: %v", m.st.AttachedClients)
	}
	m.finish(session.EndedByExit, nil)
	p.clients(4)
	if m.st.AttachedClients != nil {
		t.Fatalf("count recorded after the outcome: %v", *m.st.AttachedClients)
	}
}

// TestPTYURLPattern: the plan's url_pattern is matched against terminal
// output with escapes stripped, across a split write, and with nothing
// after it (taken once the output has been quiet).
func TestPTYURLPattern(t *testing.T) {
	r := startPTY(t, []string{"FAKE_PTY_ARGV=1"}, func(p *session.Plan) {
		p.URLPattern = `https://claude\.ai/code/session_[A-Za-z0-9_-]+`
		p.Env = []string{"FROM_PROFILE=1"}
	})
	r.running()
	c := r.attach(attach.Size{})
	c.Send([]byte("url\n"))
	waitFor(t, "remote_url", func() bool {
		return r.status().RemoteURL == "https://claude.ai/code/session_01FAKEPTYSESSION0001"
	})
	if u := r.status().RemoteURL; u != "https://claude.ai/code/session_01FAKEPTYSESSION0001" {
		t.Fatalf("remote_url %q", u)
	}
}

// TestAgentEnvPlanLayer: the plan's env (from the profile) comes after
// env_base and before the env-file and --env.
func TestAgentEnvPlanLayer(t *testing.T) {
	m := newTestMonitor(t)
	m.plan.Env = []string{"A=plan", "B=plan", "P=plan"}
	env, err := m.agentEnv()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["A"] != "plan" || got["B"] != "file" || got["C"] != "flag" || got["P"] != "plan" {
		t.Fatalf("env %v", env)
	}
}
