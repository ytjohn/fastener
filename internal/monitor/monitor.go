// Package monitor is the body of the hidden `fastener _monitor` command: one
// monitor per session generation. It spawns the agent from the launch plan,
// records output and parser fields, enforces limits, turns signals into
// recorded facts, and is the only writer of status.json while it lives.
package monitor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"slices"
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
// errors). The pump accumulates at most maxParseLine bytes of a line, so this
// caps memory as well as parsing; the rest of an overlong line is streamed to
// output.log without being held. A line buffer that grew past keepLineCap is
// released after its line instead of being kept for reuse.
// final_message is capped so status.json stays small.
const (
	maxParseLine    = 16 << 20
	keepLineCap     = 1 << 20
	maxFinalMessage = 64 << 10
)

type monitor struct {
	// maxHeld is the longest partial line pumpDedupe has held, for tests
	// that check redraws are never accumulated into one line.
	maxHeld int

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

	// writeStatus and retryDelay are overridable for tests.
	writeStatus func(path string, v any) error
	retryDelay  time.Duration
}

const finalWriteAttempts = 5

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
	write, delay := m.writeStatus, m.retryDelay
	if write == nil {
		write = session.WriteJSON
	}
	if delay == 0 {
		delay = 250 * time.Millisecond
	}
	// The exited snapshot is the only record of how the agent ended, so a
	// failed write is retried (about 1s in all) before giving up.
	attempts := 1
	if st.State == session.StateExited {
		attempts = finalWriteAttempts
	}
	for i := 1; i <= attempts; i++ {
		err := write(m.d.StatusPath(), &st)
		if err == nil {
			break
		}
		m.log.Printf("write status (attempt %d/%d): %v", i, attempts, err)
		if i < attempts {
			time.Sleep(delay)
		}
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
		// The attach socket goes with the monitor, on every path to an
		// outcome (also a stop or launch failure before the spawn).
		s.AttachSocket, s.AttachedClients = "", nil
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
	c.AttachedClients = clonePtr(s.AttachedClients)
	c.AgentError = clonePtr(s.AgentError)
	c.RemoteSessions = slices.Clone(s.RemoteSessions)
	if s.Usage != nil {
		u := *s.Usage
		u.InputTokens = clonePtr(s.Usage.InputTokens)
		u.OutputTokens = clonePtr(s.Usage.OutputTokens)
		u.CostUSD = clonePtr(s.Usage.CostUSD)
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
	layers := [][]string{m.sess.EnvBase, m.plan.Env}
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
	var urls *urlWatch
	if m.plan.URLPattern != "" {
		re, err := regexp.Compile(m.plan.URLPattern)
		if err != nil {
			return m.launchFailed(fmt.Errorf("url_pattern: %w", err))
		}
		urls = &urlWatch{m: m, sm: parser.NewStreamMatcher(re)}
	}
	cmd := exec.Command(m.plan.Argv[0], m.plan.Argv[1:]...)
	cmd.Dir = m.sess.Cwd
	cmd.Env = env
	cmd.Stderr = errLog
	cmd.SysProcAttr = agentSysProcAttr()
	if m.plan.PTY != nil && m.plan.PromptMode == "stdin" {
		return m.launchFailed(errors.New("prompt_mode stdin cannot be used with a pty (the terminal is the agent's stdin)"))
	}
	if m.plan.PromptMode == "stdin" {
		in, err := os.Open(m.d.PromptPath())
		if err != nil {
			return m.launchFailed(err)
		}
		defer in.Close()
		cmd.Stdin = in
	}
	// pr is what the monitor reads the agent's output from; pw is the
	// agent's end, closed here once the agent holds it. Headless: a pipe for
	// stdout (stderr goes to stderr.log). Interactive: the pty, which is the
	// agent's controlling terminal and all of stdin, stdout and stderr.
	var pr io.ReadCloser
	var pw *os.File
	var pt *ptyIO
	if m.plan.PTY != nil {
		pt, err = m.openPTY()
		if err != nil {
			return m.launchFailed(fmt.Errorf("pty: %w", err))
		}
		defer pt.close() // after the outcome is recorded: clients get it in the exit frame
		pr, pw = pt.master, pt.slave
		cmd.Stdin, cmd.Stdout, cmd.Stderr = pw, pw, pw
		cmd.SysProcAttr = agentPTYSysProcAttr()
	} else {
		r, w, err := os.Pipe()
		if err != nil {
			return m.launchFailed(err)
		}
		pr, pw = r, w
		cmd.Stdout = pw
	}

	// Linux delivers Pdeathsig when the spawning *thread* exits (golang/go#27505),
	// so spawn and wait on one locked thread that lives as long as the agent.
	// macOS has no Pdeathsig but uses the same shape; on both, cmd.Wait is the
	// only reaper.
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
	exitPending := make(chan struct{}, 1) // agent is dead; still an unreaped zombie when pinsZombie
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
	if pt != nil {
		pt.spawned(pid)
	}
	startedAt := time.Now().UTC()
	m.update(func(s *session.Status) {
		s.State, s.Agent, s.PGID, s.StartedAt = session.StateRunning, &agent, pid, &startedAt
		if pt != nil {
			n := pt.srv.Clients()
			s.AttachSocket, s.AttachedClients = pt.path, &n
		}
	})
	// The conversation an interactive agent writes is looked up on a
	// schedule, and once more at exit (below).
	conv := m.newConversationFinder(env, startedAt)
	stopFind, findDone := make(chan struct{}), make(chan struct{})
	if conv != nil {
		go conv.run(stopFind, findDone)
	} else {
		close(findDone)
	}

	var src io.Reader = pr
	var taps []io.Writer // each never blocks or fails
	if pt != nil {
		taps = append(taps, pt.srv)
	}
	if urls != nil {
		taps = append(taps, urls)
	}
	if len(taps) > 0 {
		src = io.TeeReader(pr, io.MultiWriter(taps...))
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer pr.Close()
		m.pump(src, out, p)
	}()
	stopFlush := make(chan struct{})
	go m.flushLoop(stopFlush, urls)

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
			// A stop request during the limit's grace can shorten it, but the
			// limit is what ended the session.
			m.terminate(cmd, pid, time.Duration(m.sess.Grace), time.Time{}, exitPending, sigs)
		}
	case sig := <-sigs:
		if !exitedAlready() {
			grace := m.requestGrace(session.StopRequest{})
			endedBy = session.EndedBySignal
			var last time.Time
			if req, ok := m.stopRequest(); ok {
				endedBy = session.EndedByStop
				grace, last = m.requestGrace(req), req.RequestedAt
			}
			m.log.Printf("received %v; ended_by=%s grace=%s", sig, endedBy, grace)
			if m.terminate(cmd, pid, grace, last, exitPending, sigs) {
				endedBy = session.EndedByStop
			}
		}
	}
	if pt != nil {
		pt.reaping()
	}
	close(reapNow) // signalling is finished; let the spawn goroutine reap
	ps := <-exited

	// Abandoning remaining output after drainTimeout is deliberate: a
	// descendant holding the pipe must not keep the session from finishing.
	// Anything it writes later never reaches output.log; status.json says so.
	truncated := false
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		truncated = true
		m.log.Printf("stdout still open %s after agent exit (a descendant holds it); not waiting, later output is lost", drainTimeout)
	}
	close(stopFlush)
	close(stopFind)
	<-findDone
	if conv != nil {
		conv.lookup() // the agent has exited: whatever it wrote is there now
	}
	m.finish(endedBy, func(s *session.Status) {
		exitFacts(s, ps)
		s.OutputTruncated = truncated
	})
	return 0
}

// requestGrace is the grace period a stop request asks for, else the session's.
func (m *monitor) requestGrace(req session.StopRequest) time.Duration {
	if req.Grace > 0 {
		return time.Duration(req.Grace)
	}
	return time.Duration(m.sess.Grace)
}

// terminate sends SIGTERM to the agent's process group, waits up to grace for
// the agent to die, then SIGKILLs the group. The agent is not reaped until the
// caller closes reapNow, so (with pinsZombie) its pgid cannot be reused
// meanwhile. It
// returns once exitPending has been consumed.
//
// While it waits, a further signal with a stop request newer than last
// (requested_at) can shorten the wait to that request's requested_at + grace;
// it never extends it. The first deadline is monotonic (receipt + grace); a
// later one is wall-clock, so a clock step can move it. terminate reports
// whether it saw such a request, whether or not it shortened the wait.
func (m *monitor) terminate(cmd *exec.Cmd, pgid int, grace time.Duration, last time.Time,
	exitPending <-chan struct{}, sigs <-chan os.Signal) (seen bool) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	timer := time.NewTimer(grace)
	defer timer.Stop()
wait:
	for {
		select {
		case <-exitPending:
			if pinsZombie {
				_ = syscall.Kill(-pgid, syscall.SIGKILL) // reach lingering group members
			}
			return seen
		case <-timer.C:
			break wait
		case sig := <-sigs:
			req, ok := m.stopRequest()
			if !ok || !req.RequestedAt.After(last) {
				m.log.Printf("received %v during termination; no newer stop request", sig)
				continue
			}
			last, seen = req.RequestedAt, true
			if d := req.RequestedAt.Add(m.requestGrace(req)); d.Before(deadline) {
				deadline = d
				timer.Reset(time.Until(d)) // Go 1.23+: Reset needs no drain
			}
			m.log.Printf("received %v during termination; stop request at %s, killing at %s",
				sig, req.RequestedAt.Format(time.RFC3339Nano), deadline.UTC().Format(time.RFC3339Nano))
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Process.Kill() // covers a leader that left the group
	select {
	case <-exitPending:
	case <-time.After(30 * time.Second):
		m.log.Printf("agent still alive after SIGKILL; still waiting for it to exit")
		<-exitPending
	}
	return seen
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

// lineSafe feeds one line to the parser, recovering a panic; callers hold m.mu.
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

// pump copies agent stdout to output.log unchanged and feeds the parser one
// line at a time. Every byte is written through as it is read. A line is
// accumulated for the parser only up to maxParseLine bytes; once it is longer
// the pump stops accumulating, keeps writing the rest straight through,
// counts one parse error, and resumes parsing after the next newline. So
// memory stays bounded however long a line is.
//
// With the plan's dedupe_frames the pump is pumpDedupe instead.
func (m *monitor) pump(r io.Reader, out io.Writer, p parser.Parser) {
	if m.plan.DedupeFrames {
		m.pumpDedupe(r, out, p)
		return
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	overlong := false
	for {
		chunk, err := br.ReadSlice('\n') // valid only until the next read
		if len(chunk) > 0 {
			_, _ = out.Write(chunk)
			if !overlong {
				if len(line)+len(chunk) > maxParseLine {
					overlong, line = true, nil
				} else {
					line = appendCapped(line, chunk)
				}
			}
		}
		if err == bufio.ErrBufferFull {
			continue // the line goes on
		}
		if err == nil || len(line) > 0 || overlong { // a newline, or a last unterminated line
			m.endLine(p, line, overlong)
		}
		if err != nil {
			return
		}
		overlong = false
		line = resetLine(line)
	}
}

// resetLine empties a line buffer for reuse, releasing one that grew past
// keepLineCap.
func resetLine(line []byte) []byte {
	if cap(line) > keepLineCap {
		return nil
	}
	return line[:0]
}

// pumpDedupe is pump for a remote bridge, which redraws its status screen
// every few seconds: written through unchanged, output.log would grow without
// bound while saying nothing new. So:
//
//   - A line ends at "\n" or at a "\r" not followed by "\n" (a carriage-return
//     redraw), and every line is parsed.
//   - Each line is held until it ends, then written unless frameDedupe calls
//     it a repeat. A written line keeps its raw bytes, colours and OSC 8
//     links included, except that leading cursor-movement and erase
//     sequences are removed (see trimCursor), so `logs` on a terminal does
//     not erase earlier, unrelated lines.
//   - The held line is the one the parser accumulates anyway, so memory is
//     bounded the same way: a line that outgrows maxParseLine is written as
//     held, the rest streamed through raw, and it counts one parse error.
func (m *monitor) pumpDedupe(r io.Reader, out io.Writer, p parser.Parser) {
	br := bufio.NewReaderSize(r, 64*1024)
	fd := &frameDedupe{}
	var line []byte
	overlong := false
	pendingCR := false // the last byte read was "\r"; whether it ends the line depends on the next byte
	end := func() {
		if overlong {
			fd.unique() // written already; nothing repeats it
		} else if !fd.repeat(line) {
			_, _ = out.Write(trimCursor(line))
		}
		m.endLine(p, line, overlong)
		overlong = false
		line = resetLine(line)
	}
	add := func(seg []byte) {
		switch {
		case overlong:
			_, _ = out.Write(seg)
		case len(line)+len(seg) > maxParseLine:
			_, _ = out.Write(line) // stop holding: write what was held, then stream
			_, _ = out.Write(seg)
			overlong, line = true, nil
		default:
			line = appendCapped(line, seg)
			if len(line) > m.maxHeld {
				m.maxHeld = len(line)
			}
		}
	}
	for {
		chunk, err := br.ReadSlice('\n') // valid only until the next read
		if pendingCR && len(chunk) > 0 {
			pendingCR = false
			if chunk[0] != '\n' {
				end()
			}
		}
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\r')
			if i < 0 {
				add(chunk)
				break
			}
			add(chunk[:i+1])
			chunk = chunk[i+1:]
			if len(chunk) == 0 {
				pendingCR = true // decided by the next read
				break
			}
			if chunk[0] != '\n' {
				end() // a lone "\r"
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue // the line goes on
		case err == nil:
			end() // a newline
		default:
			if len(line) > 0 || overlong || pendingCR { // a last unterminated line
				end()
			}
			return
		}
	}
}

// cursorRE matches cursor-movement and erase sequences (CSI n A-H, J, K) at
// the start of a line.
var cursorRE = regexp.MustCompile("^(?:\x1b\\[[0-9;]*[A-HJK])+")

// trimCursor drops leading cursor-movement and erase sequences from line.
func trimCursor(line []byte) []byte {
	if n := cursorRE.Find(line); n != nil {
		return line[len(n):]
	}
	return line
}

// frameMarkerRE recognizes the first line of a redrawn frame: Claude moves
// the cursor up over the previous frame (ESC [ n A, then ESC [ J) at the
// start of a line.
var frameMarkerRE = regexp.MustCompile("^\x1b\\[[0-9]*A")

// dedupeMaxLine: longer lines are never de-duplicated (always written), so
// the stripped copy hashed for each line stays small.
const dedupeMaxLine = 64 << 10

// dedupeMaxFrame caps how many lines of one frame are remembered; lines past
// it are compared with nothing and written. A Claude bridge's screen is about
// 8 lines plus one per session (32 by default).
const dedupeMaxFrame = 1024

// frameKey identifies a line by the SHA-256 of its text with terminal escapes
// stripped (redraws of the same line differ in cursor escapes). ok is false
// for a line that is never de-duplicated, so it equals nothing.
type frameKey struct {
	sum [sha256.Size]byte
	ok  bool
}

func (a frameKey) same(b frameKey) bool { return a.ok && b.ok && a.sum == b.sum }

// frameDedupe recognizes a redrawn status screen by comparing each frame
// with the previous one, position by position. A frame starts at a line that
// begins with a cursor-up sequence (frameMarkerRE); a line is a repeat only
// if it equals the line at the same position in the previous frame. So a
// screen that changes and changes back (Connected, Disconnected, Connected)
// logs every change, while a steady screen logs nothing after its first
// frame. Until a frame marker is seen, a line is a repeat only if it equals
// the line right before it. Memory is two frames of hashes, at most
// 2 × dedupeMaxFrame keys.
type frameDedupe struct {
	framed    bool       // a frame marker has been seen
	prev, cur []frameKey // the previous and the current frame, in order
	last      frameKey   // the line before this one
	hadLast   bool
}

// repeat reports whether line (with or without its terminator) repeats the
// line at its position in the previous frame (or, before any frame marker,
// the previous line), and records it.
func (d *frameDedupe) repeat(line []byte) bool {
	text := bytes.TrimRight(line, "\r\n")
	var key frameKey
	if len(text) <= dedupeMaxLine {
		key = frameKey{sum: sha256.Sum256(parser.StripTerminal(text)), ok: true}
	}
	return d.record(key, frameMarkerRE.Match(text))
}

// unique records a line that repeats nothing and that nothing repeats (an
// overlong line, already written).
func (d *frameDedupe) unique() { d.record(frameKey{}, false) }

func (d *frameDedupe) record(key frameKey, marker bool) bool {
	if marker {
		d.framed = true
		d.prev, d.cur = d.cur, d.prev[:0]
	}
	pos := len(d.cur)
	var dup bool
	if d.framed {
		dup = pos < len(d.prev) && d.prev[pos].same(key)
	} else {
		dup = d.hadLast && d.last.same(key)
	}
	// The lines before the first marker are a frame too: the first redraw
	// is compared with them.
	if pos < dedupeMaxFrame {
		d.cur = append(d.cur, key)
	}
	d.last, d.hadLast = key, true
	return dup
}

// appendCapped appends chunk to line, growing it by doubling but never past
// maxParseLine (the caller guarantees the result fits).
func appendCapped(line, chunk []byte) []byte {
	need := len(line) + len(chunk)
	if need > cap(line) {
		n := max(2*cap(line), need, 4096)
		n = min(n, maxParseLine)
		grown := make([]byte, len(line), n)
		copy(grown, line)
		line = grown
	}
	return append(line, chunk...)
}

// endLine records one complete line: a parse error if it was overlong, else
// the parser's update.
func (m *monitor) endLine(p parser.Parser, line []byte, overlong bool) {
	if overlong {
		m.log.Printf("stdout line longer than %d bytes: written to output.log, not parsed", maxParseLine)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if overlong {
		m.st.ParseErrors++
		m.dirty = true
		return
	}
	if !m.lineSafe(p, bytes.TrimRight(line, "\r\n")) {
		return
	}
	if len(m.st.FinalMessage) > maxFinalMessage {
		i := maxFinalMessage
		for i > 0 && !utf8.RuneStart(m.st.FinalMessage[i]) {
			i-- // never split a UTF-8 sequence
		}
		m.st.FinalMessage = m.st.FinalMessage[:i]
	}
	m.dirty = true
}

// flushLoop writes parser updates at most once per second; it also lets a
// url watch take a match held at the end of quiet output.
func (m *monitor) flushLoop(stop <-chan struct{}, urls *urlWatch) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if urls != nil {
				urls.flush()
			}
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

// urlWatch matches the plan's url_pattern against the agent's output stream
// and records the latest match as remote_url (flushed with parser fields).
type urlWatch struct {
	m  *monitor
	mu sync.Mutex
	sm *parser.StreamMatcher
}

func (u *urlWatch) Write(p []byte) (int, error) {
	u.mu.Lock()
	found := u.sm.Feed(p)
	u.mu.Unlock()
	u.record(found)
	return len(p), nil
}

func (u *urlWatch) flush() {
	u.mu.Lock()
	found := u.sm.Flush()
	u.mu.Unlock()
	u.record(found)
}

func (u *urlWatch) record(url string) {
	if url == "" {
		return
	}
	m := u.m
	m.mu.Lock()
	defer m.mu.Unlock()
	// The matcher already refuses cut-short copies (parser.StreamMatcher).
	if m.st.RemoteURL != url {
		m.st.RemoteURL = url
		m.dirty = true
	}
}
