package monitor

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/attach"
	"github.com/ytjohn/fastener/internal/finder"
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
	// fakeagent installs its SIGWINCH handler before its first line; a
	// client attaching earlier could resize it before it listens.
	waitFor(r.t, "the agent's first line", func() bool {
		b, _ := os.ReadFile(r.d.OutputLog())
		return bytes.Contains(b, []byte("tty="))
	})
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
// changes nothing for the kernel, and an Ink TUI ignores a bare SIGWINCH, so
// the monitor nudges: one column narrower, then back. The agent sees both
// sizes, and the welcome reports the client's size, not the nudged one.
func TestPTYAttachSameSizeRedraws(t *testing.T) {
	r := startPTY(t, nil, nil)
	r.running()
	c := r.attach(attach.Size{Rows: session.DefaultPTYRows, Cols: session.DefaultPTYCols})
	if got := c.Welcome.Size; got != (attach.Size{Rows: session.DefaultPTYRows, Cols: session.DefaultPTYCols}) {
		t.Fatalf("welcome size %+v", got)
	}
	recv(t, c, "winch=24x79")
	recv(t, c, "winch=24x80")
}

// TestPTYResizeDuringNudge: a resize that arrives while a nudge holds the pty
// narrower wins; the nudge's delayed restore does not undo it.
func TestPTYResizeDuringNudge(t *testing.T) {
	r := startPTY(t, nil, nil)
	r.running()
	c := r.attach(attach.Size{Rows: session.DefaultPTYRows, Cols: session.DefaultPTYCols})
	if err := c.Resize(attach.Size{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	recv(t, c, "winch=30x100")
	time.Sleep(2 * nudgeDelay)
	if err := c.Send([]byte("size\n")); err != nil {
		t.Fatal(err)
	}
	recv(t, c, "size=30x100")
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
	waitIgnoresTerm(t, r.d)
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

// TestPTYConversationFinder: the monitor records the conversation the
// plan's finder resolves, here an assigned ID whose transcript appears in a
// synthetic Claude store (CLAUDE_CONFIG_DIR) after a while, and the
// not-yet-written state before that.
func TestPTYConversationFinder(t *testing.T) {
	old := findSchedule
	oldEvery := findEvery
	findSchedule, findEvery = []time.Duration{50 * time.Millisecond, 200 * time.Millisecond}, 100*time.Millisecond
	t.Cleanup(func() { findSchedule, findEvery = old, oldEvery })
	conf := t.TempDir()
	const sid = "11111111-2222-4333-8444-555555555555"
	r := startPTY(t, []string{"CLAUDE_CONFIG_DIR=" + conf}, func(p *session.Plan) {
		p.Finder, p.SessionID = "claude-transcripts", sid
	})
	r.running()
	waitFor(t, "conversation_error", func() bool { return strings.Contains(r.status().ConversationError, "no transcript") })
	if st := r.status(); st.ConversationID != "" {
		t.Fatalf("found before it exists: %+v", st)
	}
	var sess session.Session
	if err := session.ReadJSON(r.d.SessionPath(), &sess); err != nil {
		t.Fatal(err)
	}
	cwd, _ := filepath.EvalSymlinks(sess.Cwd)
	dir := filepath.Join(conf, "projects", finderKeyForTest(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "conversation_id", func() bool { return r.status().ConversationID == sid })
	st := r.status()
	if st.ConversationSource != "session_id" || st.ConversationError != "" {
		t.Fatalf("status %+v", st)
	}
	c := r.attach(attach.Size{})
	c.Send([]byte("exit 0\n"))
	if fin := r.wait(); fin.ConversationID != sid || fin.ConversationSource != "session_id" {
		t.Fatalf("final status %+v", fin)
	}
}

// TestConversationLookupAtExit: a conversation written after the last
// scheduled lookup is still recorded, by the lookup at exit.
func TestConversationLookupAtExit(t *testing.T) {
	old := findSchedule
	findSchedule = []time.Duration{time.Hour}
	t.Cleanup(func() { findSchedule = old })
	conf := t.TempDir()
	const sid = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	r := startPTY(t, []string{"CLAUDE_CONFIG_DIR=" + conf}, func(p *session.Plan) {
		p.Finder, p.SessionID = "claude-transcripts", sid
	})
	r.running()
	var sess session.Session
	_ = session.ReadJSON(r.d.SessionPath(), &sess)
	cwd, _ := filepath.EvalSymlinks(sess.Cwd)
	dir := filepath.Join(conf, "projects", finderKeyForTest(cwd))
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("{}\n"), 0o600)
	c := r.attach(attach.Size{})
	c.Send([]byte("exit 0\n"))
	if fin := r.wait(); fin.ConversationID != sid {
		t.Fatalf("final status %+v", fin)
	}
}

// finderKeyForTest is Claude's project key for a plain ASCII path.
func finderKeyForTest(p string) string {
	b := []byte(p)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// convTest is a conversationFinder on a test monitor whose agent runs.
func convTest(t *testing.T, plan func(*session.Plan), find finder.Finder) *conversationFinder {
	t.Helper()
	m := newTestMonitor(t)
	m.st.State = session.StateRunning
	if plan != nil {
		plan(&m.plan)
	}
	q := finder.Query{Cwd: t.TempDir(), StartedAt: time.Now(), SessionID: m.plan.SessionID}
	return &conversationFinder{m: m, find: find, q: q, exact: finder.Exact(q)}
}

func convState(c *conversationFinder) (id, cand, source, msg string) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	s := c.m.st
	return s.ConversationID, s.ConversationCandidate, s.ConversationSource, s.ConversationError
}

// TestCodexScenarioB: two Codex sessions in one folder, the later one
// writing first. The first rollout is only a candidate; when ours appears
// too, the candidate is cleared for good and the error names both.
func TestCodexScenarioB(t *testing.T) {
	codex := t.TempDir()
	var c *conversationFinder
	c = convTest(t, func(p *session.Plan) { p.Finder = "codex-rollouts" }, nil)
	f, _ := finder.Get("codex-rollouts")
	c.find = f
	c.q.Env = []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + codex}
	c.q.StartedAt = time.Now().Add(-5 * time.Minute)
	physical, _ := filepath.EvalSymlinks(c.q.Cwd)
	day := filepath.Join(codex, "sessions", time.Now().Format("2006/01/02"))
	rollout := func(id string, at time.Time) {
		t.Helper()
		_ = os.MkdirAll(day, 0o700)
		meta := `{"type":"session_meta","payload":{"id":"` + id + `","timestamp":"` + at.UTC().Format(time.RFC3339Nano) +
			`","cwd":"` + physical + `","source":"cli"}}` + "\n"
		if err := os.WriteFile(filepath.Join(day, "rollout-x-"+id+".jsonl"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const other, ours = "bbbbbbbb-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-000000000001"
	c.lookup(false)
	if id, cand, _, msg := convState(c); id != "" || cand != "" || !strings.Contains(msg, "no conversation") {
		t.Fatalf("nothing written: %q %q %q", id, cand, msg)
	}
	rollout(other, c.q.StartedAt.Add(3*time.Minute)) // the later session writes first
	if c.lookup(false) {
		t.Fatal("a heuristic answer ended the lookups")
	}
	if id, cand, src, _ := convState(c); id != "" || cand != other || src != "codex-rollouts" {
		t.Fatalf("one rollout: id %q candidate %q source %q", id, cand, src)
	}
	rollout(ours, c.q.StartedAt.Add(time.Minute))
	c.lookup(false)
	id, cand, _, msg := convState(c)
	if id != "" || cand != "" || !strings.Contains(msg, other) || !strings.Contains(msg, ours) {
		t.Fatalf("two rollouts: id %q candidate %q error %q", id, cand, msg)
	}
	// Even if one of them goes away, the session stays ambiguous.
	_ = os.Remove(filepath.Join(day, "rollout-x-"+other+".jsonl"))
	c.lookup(true)
	if id, cand, _, msg := convState(c); id != "" || cand != "" || !strings.Contains(msg, "2 conversations") {
		t.Fatalf("after one went away: %q %q %q", id, cand, msg)
	}
}

// TestCandidateChangesIsAmbiguous: a different single candidate later (the
// first one gone, another there) is ambiguous too, and a candidate gone at
// exit is not kept.
func TestCandidateChangesIsAmbiguous(t *testing.T) {
	answers := []string{"one", "two"}
	c := convTest(t, func(p *session.Plan) { p.Finder = "pi-sessions" }, func(finder.Query) (string, error) {
		a := answers[0]
		answers = answers[1:]
		return a, nil
	})
	c.lookup(false)
	if _, cand, _, _ := convState(c); cand != "one" {
		t.Fatalf("candidate %q", cand)
	}
	c.lookup(false)
	if _, cand, _, msg := convState(c); cand != "" || !strings.Contains(msg, "one, two") {
		t.Fatalf("changed candidate: %q %q", cand, msg)
	}

	gone := convTest(t, func(p *session.Plan) { p.Finder = "pi-sessions" }, nil)
	calls := 0
	gone.find = func(finder.Query) (string, error) {
		calls++
		if calls == 1 {
			return "only", nil
		}
		return "", finder.ErrNotFound
	}
	gone.lookup(false)
	gone.lookup(true)
	if _, cand, _, msg := convState(gone); cand != "" || !strings.Contains(msg, "gone") {
		t.Fatalf("candidate gone at exit: %q %q", cand, msg)
	}
}

// TestExactNeverCandidate: an assigned ID is recorded as conversation_id and
// ends the lookups; a heuristic answer never is.
func TestExactNeverCandidate(t *testing.T) {
	c := convTest(t, func(p *session.Plan) { p.Finder, p.SessionID = "claude-transcripts", "sid-1" },
		func(q finder.Query) (string, error) { return q.SessionID, nil })
	if !c.lookup(false) {
		t.Fatal("an exact answer did not end the lookups")
	}
	if id, cand, src, _ := convState(c); id != "sid-1" || cand != "" || src != "session_id" {
		t.Fatalf("exact: %q %q %q", id, cand, src)
	}
	h := convTest(t, func(p *session.Plan) { p.Finder = "claude-transcripts" },
		func(finder.Query) (string, error) { return "guess", nil })
	h.lookup(false)
	h.lookup(true)
	if id, cand, _, _ := convState(h); id != "" || cand != "guess" {
		t.Fatalf("heuristic: id %q candidate %q", id, cand)
	}
}

// TestExitLookupBounded: a finder that hangs does not hold up the exit.
func TestExitLookupBounded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := convTest(t, func(p *session.Plan) { p.Finder = "codex-rollouts" },
		func(finder.Query) (string, error) { <-release; return "", finder.ErrNotFound })
	start := time.Now()
	c.atExit()
	if d := time.Since(start); d > exitLookupTimeout+time.Second {
		t.Fatalf("exit lookup took %v", d)
	}
}
