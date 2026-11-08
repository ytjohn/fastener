// Package monitor is the body of the hidden `fastener _monitor` command: one
// monitor per session generation. It spawns the agent from the launch plan,
// records output and parser fields, enforces limits, turns signals into
// recorded facts, and is the only writer of status.json while it lives.
package monitor

import (
	"bufio"
	"bytes"
	"fmt"
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

	mu    sync.Mutex
	st    session.Status
	dirty bool
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

// write persists status; callers hold m.mu.
func (m *monitor) write() {
	m.st.SchemaVersion = session.SchemaVersion
	m.st.UpdatedAt = time.Now().UTC()
	if err := session.WriteJSON(m.d.StatusPath(), &m.st); err != nil {
		m.log.Printf("write status: %v", err)
	}
	m.dirty = false
}

func (m *monitor) finish(endedBy string, mutate func(*session.Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	m.st.State, m.st.EndedBy, m.st.EndedAt = session.StateExited, endedBy, &now
	if mutate != nil {
		mutate(&m.st)
	}
	m.write()
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
	m.mu.Lock()
	m.st.State = session.StateStarting
	m.write()
	m.mu.Unlock()
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
		m.finish(session.EndedBySignal, nil)
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
	m.mu.Lock()
	now := time.Now().UTC()
	m.st.State, m.st.Agent, m.st.PGID, m.st.StartedAt = session.StateRunning, &agent, pid, &now
	m.write()
	m.mu.Unlock()

	p, ok := parser.Get(m.plan.Parser)
	if !ok {
		m.log.Printf("unknown parser %q; falling back to none", m.plan.Parser)
		p, _ = parser.Get("none")
	}
	drained := make(chan struct{})
	go m.pump(pr, out, p, drained)
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
			return false
		}
	}
	gone := false // exitPending already consumed
	select {
	case <-exitPending:
		gone = true
	case <-limit:
		if gone = exitedAlready(); !gone {
			endedBy = session.EndedByLimit
			gone = m.terminate(cmd, pid, time.Duration(m.sess.Grace), exitPending)
		}
	case sig := <-sigs:
		if gone = exitedAlready(); !gone {
			grace := time.Duration(m.sess.Grace)
			endedBy = session.EndedBySignal
			if req, ok := m.stopRequest(); ok {
				endedBy = session.EndedByStop
				if req.Grace > 0 {
					grace = time.Duration(req.Grace)
				}
			}
			m.log.Printf("received %v; ended_by=%s grace=%s", sig, endedBy, grace)
			gone = m.terminate(cmd, pid, grace, exitPending)
		}
	}
	_ = gone
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
		m.log.Printf("agent did not exit 30s after SIGKILL")
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

func (m *monitor) pump(r *os.File, out *os.File, p parser.Parser, done chan<- struct{}) {
	defer close(done)
	defer r.Close()
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
			m.mu.Lock()
			if m.dirty {
				m.write()
			}
			m.mu.Unlock()
		}
	}
}
