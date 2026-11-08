// Package monitor is the body of the hidden `fastener _monitor` command: one
// monitor per session generation. It spawns the agent from the launch plan,
// records output and parser fields, enforces limits, turns signals into
// recorded facts, and is the only writer of status.json while it lives.
package monitor

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/ytjohn/fastener/internal/parser"
	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/runenv"
	"github.com/ytjohn/fastener/internal/session"
)

// drainTimeout bounds how long the monitor waits for stdout EOF after the
// agent exits; a grandchild may hold the pipe open indefinitely.
const drainTimeout = 2 * time.Second

// Lines longer than maxParseLine are logged but not parsed (counted as parse
// errors), so a huge tool result is never unmarshalled while holding m.mu.
// (ReadBytes still allocates the whole line; this caps parsing, not memory.)
// final_message is capped so status.json stays small.
const (
	maxParseLine    = 16 << 20
	maxFinalMessage = 64 << 10
)

type monitor struct {
	d    session.Dir
	gen  int
	sess session.Session
	plan session.Plan
	log  *log.Logger

	beforeWait func(pid int) // test hook, called just before the main wait

	// mu guards the in-memory status. It is held only to mutate st or to take
	// a snapshot of it, never across a disk write.
	mu    sync.Mutex
	st    session.Status
	dirty bool   // st changed since the last snapshot
	seq   uint64 // sequence number of the last snapshot
	final bool   // the exited snapshot has been taken; no more snapshots

	// wmu serializes status.json writes. A snapshot is written only if its
	// sequence number is newer than the last one written, and nothing is
	// written after the exited snapshot.
	wmu     sync.Mutex
	written uint64
	closed  bool
}

// Run executes one generation. It returns a process exit code for the
// monitor itself (0 whenever an outcome was recorded).
func Run(dir string, gen int, logger *log.Logger) int {
	// Subscribe before anything else so a stop during startup is not lost.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	m, err := load(session.Dir(dir), gen, logger)
	if err != nil {
		logger.Printf("cannot start: %v", err)
		return 1
	}
	return m.run(sigs)
}

func load(d session.Dir, gen int, logger *log.Logger) (*monitor, error) {
	m := &monitor{d: d, gen: gen, log: logger}
	if err := session.ReadJSON(d.SessionPath(), &m.sess); err != nil {
		return nil, err
	}
	if err := session.ReadJSON(d.PlanPath(gen), &m.plan); err != nil {
		return nil, err
	}
	if len(m.plan.Argv) == 0 {
		return nil, fmt.Errorf("plan %d has empty argv", gen)
	}
	boot, err := proc.BootID()
	if err != nil {
		return nil, err
	}
	self, err := proc.Self()
	if err != nil {
		return nil, err
	}
	m.st = session.Status{Generation: gen, Monitor: &self, BootID: boot} // fresh: nothing carried over from a previous generation
	return m, nil
}

// snapshotLocked stamps st and returns a deep copy of it with a new sequence
// number; callers hold m.mu. It returns ok=false once the final (exited)
// snapshot has been taken, so nothing can follow it.
func (m *monitor) snapshotLocked() (st session.Status, seq uint64, ok bool) {
	if m.final {
		return session.Status{}, 0, false
	}
	m.st.SchemaVersion = session.SchemaVersion
	m.st.UpdatedAt = time.Now().UTC()
	m.seq++
	m.dirty = false
	if m.st.State == session.StateExited {
		m.final = true
	}
	return cloneStatus(m.st), m.seq, true
}

// persist writes a snapshot to status.json; callers must not hold m.mu.
// Writes are serialized; a snapshot older than the last one written is
// dropped, and nothing is written after the exited snapshot.
func (m *monitor) persist(st session.Status, seq uint64) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if m.closed || seq <= m.written {
		return
	}
	if err := session.WriteJSON(m.d.StatusPath(), &st); err != nil {
		m.log.Printf("write status: %v", err)
	}
	m.written = seq
	if st.State == session.StateExited {
		m.closed = true
	}
}

// update applies mutate to the status under m.mu, then writes a snapshot
// outside it.
func (m *monitor) update(mutate func(*session.Status)) {
	m.mu.Lock()
	mutate(&m.st)
	st, seq, ok := m.snapshotLocked()
	m.mu.Unlock()
	if ok {
		m.persist(st, seq)
	}
}

// flush writes the status if the parser changed it since the last snapshot.
func (m *monitor) flush() {
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return
	}
	st, seq, ok := m.snapshotLocked()
	m.mu.Unlock()
	if ok {
		m.persist(st, seq)
	}
}

func (m *monitor) finish(endedBy string, mutate func(*session.Status)) {
	m.update(func(s *session.Status) {
		now := time.Now().UTC()
		s.State, s.EndedBy, s.EndedAt = session.StateExited, endedBy, &now
		if mutate != nil {
			mutate(s)
		}
	})
}

// cloneStatus copies s, including the values its pointer fields reference,
// so a snapshot shares no memory with the live status (parsers update
// pointed-to values such as Usage in place).
func cloneStatus(s session.Status) session.Status {
	c := s
	c.Monitor = clonePtr(s.Monitor)
	c.Agent = clonePtr(s.Agent)
	c.StartedAt = clonePtr(s.StartedAt)
	c.EndedAt = clonePtr(s.EndedAt)
	c.ExitCode = clonePtr(s.ExitCode)
	c.AgentError = clonePtr(s.AgentError)
	if s.Usage != nil {
		u := *s.Usage
		u.InputTokens = clonePtr(s.Usage.InputTokens)
		u.OutputTokens = clonePtr(s.Usage.OutputTokens)
		c.Usage = &u
	}
	return c
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// stopRequest reports a stop request addressed to this generation.
func (m *monitor) stopRequest() (session.StopRequest, bool) {
	var r session.StopRequest
	if err := session.ReadJSON(m.d.StopPath(), &r); err != nil {
		return r, false
	}
	return r, r.Generation == m.gen
}

func (m *monitor) agentEnv() ([]string, error) {
	layers := [][]string{m.sess.EnvBase}
	if m.sess.EnvFile != "" {
		f, err := runenv.ReadFile(m.sess.EnvFile)
		if err != nil {
			return nil, fmt.Errorf("env file: %w", err)
		}
		layers = append(layers, f)
	}
	layers = append(layers, m.sess.Env, []string{proc.MarkerEnv + "=" + m.plan.Marker})
	return runenv.Merge(layers...), nil
}

func (m *monitor) launchFailed(err error) int {
	m.finish(session.EndedByLaunchFailed, func(s *session.Status) { s.Error = err.Error() })
	return 0
}

func (m *monitor) run(sigs <-chan os.Signal) int {
	// Identify this monitor in status.json before anything else can fail or block.
	m.update(func(s *session.Status) { s.State = session.StateStarting })
	if _, ok := m.stopRequest(); ok {
		m.finish(session.EndedByStop, nil) // stopped before the agent was spawned
		return 0
	}
	env, err := m.agentEnv()
	if err != nil {
		return m.launchFailed(err)
	}
	out, err := os.OpenFile(m.d.OutputLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return m.launchFailed(err)
	}
	defer out.Close()
	errLog, err := os.OpenFile(m.d.StderrLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return m.launchFailed(err)
	}
	defer errLog.Close()

	if fi, err := os.Stat(m.sess.Cwd); err != nil {
		return m.launchFailed(fmt.Errorf("cwd %s: %w", m.sess.Cwd, unwrapPath(err)))
	} else if !fi.IsDir() {
		return m.launchFailed(fmt.Errorf("cwd %s: not a directory", m.sess.Cwd))
	}

	p, ok := parser.Get(m.plan.Parser)
	if !ok {
		return m.launchFailed(fmt.Errorf("unknown parser %q", m.plan.Parser))
	}
	cmd := exec.Command(m.plan.Argv[0], m.plan.Argv[1:]...)
	cmd.Dir = m.sess.Cwd
	cmd.Env = env
	cmd.Stderr = errLog
	cmd.SysProcAttr = agentSysProcAttr()
	if m.plan.PromptMode == "stdin" {
		in, err := os.Open(m.d.PromptPath())
		if err != nil {
			return m.launchFailed(err)
		}
		defer in.Close()
		cmd.Stdin = in
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return m.launchFailed(err)
	}
	cmd.Stdout = pw

	// Linux delivers Pdeathsig when the spawning *thread* exits (golang/go#27505),
	// so spawn and wait on one locked thread that lives as long as the agent.
	// A stop or signal that arrived during setup is recorded without spawning.
	if _, ok := m.stopRequest(); ok {
		pw.Close()
		pr.Close()
		m.finish(session.EndedByStop, nil)
		return 0
	}
	select {
	case sig := <-sigs:
		m.log.Printf("received %v before spawn", sig)
		pw.Close()
		pr.Close()
		endedBy := session.EndedBySignal
		if _, ok := m.stopRequest(); ok {
			endedBy = session.EndedByStop
		}
		m.finish(endedBy, nil)
		return 0
	default:
	}
	started := make(chan error, 1)
	exitPending := make(chan struct{}, 1) // agent is dead; on Linux still an unreaped zombie
	reapNow := make(chan struct{})        // closed once we are done signalling the group
	exited := make(chan *os.ProcessState, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with the goroutine
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		if pinsZombie {
			waitExitNoReap(cmd.Process.Pid)
			exitPending <- struct{}{}
			<-reapNow
			_ = cmd.Wait()
		} else {
			_ = cmd.Wait()
			exitPending <- struct{}{}
		}
		exited <- cmd.ProcessState
	}()
	if err := <-started; err != nil {
		pw.Close()
		pr.Close()
		return m.launchFailed(err)
	}
	pw.Close() // the agent holds the write end now

	pid := cmd.Process.Pid
	agent, err := proc.Of(pid)
	if err != nil {
		agent = proc.Identity{PID: pid} // already exited; exit facts follow
	}
	m.update(func(s *session.Status) {
		now := time.Now().UTC()
		s.State, s.Agent, s.PGID, s.StartedAt = session.StateRunning, &agent, pid, &now
	})

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer pr.Close()
		m.pump(pr, out, p)
	}()
	stopFlush := make(chan struct{})
	go m.flushLoop(stopFlush)

	var limit <-chan time.Time
	if m.sess.MaxRuntime > 0 {
		t := time.NewTimer(time.Duration(m.sess.MaxRuntime))
		defer t.Stop()
		limit = t.C
	}

	endedBy := session.EndedByExit
	exitedAlready := func() bool {
		select {
		case <-exitPending:
			return true
		default:
		}
		if exitedNoReap(pid) {
			<-exitPending // the waiter is about to publish it; keep the reap ordering
			return true
		}
		return false
	}
	if m.beforeWait != nil {
		m.beforeWait(pid) // test hook: lets a test make exit and signal ready together
	}
	select {
	case <-exitPending:
	case <-limit:
		if !exitedAlready() {
			endedBy = session.EndedByLimit
			m.terminate(cmd, pid, time.Duration(m.sess.Grace), exitPending)
		}
	case sig := <-sigs:
		if !exitedAlready() {
			grace := time.Duration(m.sess.Grace)
			endedBy = session.EndedBySignal
			if req, ok := m.stopRequest(); ok {
				endedBy = session.EndedByStop
				if req.Grace > 0 {
					grace = time.Duration(req.Grace)
				}
			}
			m.log.Printf("received %v; ended_by=%s grace=%s", sig, endedBy, grace)
			m.terminate(cmd, pid, grace, exitPending)
		}
	}
	close(reapNow) // signalling is finished; let the spawn goroutine reap
	ps := <-exited

	// Abandoning remaining output after drainTimeout is deliberate: a
	// descendant holding the pipe must not keep the session from finishing.
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		m.log.Printf("stdout still open after agent exit (a descendant holds it); not waiting")
	}
	close(stopFlush)
	m.finish(endedBy, func(s *session.Status) { exitFacts(s, ps) })
	return 0
}

// terminate sends SIGTERM to the agent's process group, waits up to grace for
// the agent to die, then SIGKILLs the group. The agent is not reaped until the
// caller closes reapNow, so (on Linux) its pgid cannot be reused meanwhile. It
// returns once exitPending has been consumed.
func (m *monitor) terminate(cmd *exec.Cmd, pgid int, grace time.Duration, exitPending <-chan struct{}) bool {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-exitPending:
		if pinsZombie {
			_ = syscall.Kill(-pgid, syscall.SIGKILL) // reach lingering group members
		}
		return true
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Process.Kill() // covers a leader that left the group
	select {
	case <-exitPending:
	case <-time.After(30 * time.Second):
		m.log.Printf("agent still alive after SIGKILL; still waiting for it to exit")
		<-exitPending
	}
	return true
}

func exitFacts(s *session.Status, ps *os.ProcessState) {
	if ps == nil {
		return
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		s.Signal = unix.SignalName(ws.Signal())
		s.ExitCode = nil
		return
	}
	s.Signal = ""
	code := ps.ExitCode()
	s.ExitCode = &code
}

// pump copies agent stdout to output.log unchanged and feeds the parser.
func (m *monitor) lineSafe(p parser.Parser, line []byte) (changed bool) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Printf("parser panic: %v", r)
			m.st.ParseErrors++
			changed = true
		}
	}()
	return p.Line(line, &m.st.Fields)
}

func (m *monitor) pump(r io.Reader, out io.Writer, p parser.Parser) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = out.Write(line)
			m.mu.Lock()
			switch {
			case len(line) > maxParseLine:
				m.st.ParseErrors++
				m.dirty = true
			case m.lineSafe(p, bytes.TrimRight(line, "\r\n")):
				if len(m.st.FinalMessage) > maxFinalMessage {
					i := maxFinalMessage
					for i > 0 && !utf8.RuneStart(m.st.FinalMessage[i]) {
						i-- // never split a UTF-8 sequence
					}
					m.st.FinalMessage = m.st.FinalMessage[:i]
				}
				m.dirty = true
			}
			m.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// flushLoop writes parser updates at most once per second.
func (m *monitor) flushLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.flush()
		}
	}
}

// unwrapPath drops the path an *os.PathError carries, for messages that
// already name the path.
func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
