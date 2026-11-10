package monitor

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/finder"
	"github.com/ytjohn/fastener/internal/session"
)

// findSchedule is when, after the spawn, the monitor asks the plan's finder
// for the conversation; then every findEvery while the agent runs, and once
// more as it exits (bounded by exitLookupTimeout). Agents write their
// conversation at the first message, which may come much later than the
// start; each lookup is a stat or a directory listing, so even the hourly
// ones cost nothing. An exact answer (the assigned ID) ends the lookups; a
// heuristic one does not, since another candidate may still appear.
var findSchedule = []time.Duration{
	2 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
	5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
}

var findEvery = time.Hour

// exitLookupTimeout bounds the lookup at exit, so a slow store never delays
// the session's exit record.
const exitLookupTimeout = 2 * time.Second

// conversationFinder resolves an interactive generation's conversation with
// the plan's finder and records it through the monitor's status.
//
// An exact answer becomes conversation_id. A heuristic one becomes
// conversation_candidate, and stays one only while it is the only candidate
// ever seen: once a second, different one has turned up (at the same time,
// or later), the candidate is cleared and conversation_error lists them,
// for good.
type conversationFinder struct {
	m     *monitor
	find  finder.Finder
	q     finder.Query
	exact bool
	done  bool     // exact answer recorded: no more lookups
	seen  []string // heuristic candidates seen so far, in order
}

func (m *monitor) newConversationFinder(env []string, started time.Time) *conversationFinder {
	if m.plan.Finder == "" {
		return nil
	}
	f, ok := finder.Get(m.plan.Finder)
	if !ok {
		m.log.Printf("unknown finder %q; the conversation will not be found", m.plan.Finder)
		return nil
	}
	q := finder.Query{Cwd: m.sess.Cwd, StartedAt: started, Env: env, SessionID: m.plan.SessionID, Logf: m.log.Printf}
	return &conversationFinder{m: m, find: f, q: q, exact: finder.Exact(q)}
}

// lookup asks the finder once and records the outcome. It reports whether
// lookups are over (an exact answer, or the session has ended). At exit a
// candidate is kept only if the finder still names it alone.
func (c *conversationFinder) lookup(exit bool) bool {
	if c.done {
		return true
	}
	id, err := c.find(c.q)
	m := c.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.final || m.st.State == session.StateExited {
		return true
	}
	set := func(id, cand, source, msg string) {
		s := &m.st
		if s.ConversationID != id || s.ConversationCandidate != cand || s.ConversationSource != source || s.ConversationError != msg {
			s.ConversationID, s.ConversationCandidate, s.ConversationSource, s.ConversationError = id, cand, source, msg
			m.dirty = true
		}
	}
	switch {
	case err == nil && id != "" && c.exact:
		set(id, "", "session_id", "")
		c.done = true
		return true
	case err == nil && id != "":
		c.note(id)
	case errors.Is(err, finder.ErrAmbiguous):
		var ae *finder.AmbiguousError
		if errors.As(err, &ae) {
			for _, cand := range ae.IDs {
				c.note(cand)
			}
		}
		if len(c.seen) < 2 { // the finder did not name them; still ambiguous
			set("", "", m.plan.Finder, err.Error())
			return false
		}
	case err != nil && exit && len(c.seen) == 1:
		set("", "", m.plan.Finder, "the candidate "+c.seen[0]+" is gone: "+err.Error())
		return true
	case err != nil && len(c.seen) == 0:
		msg := err.Error()
		if !errors.Is(err, finder.ErrNotFound) {
			msg = "finder " + m.plan.Finder + ": " + msg
		}
		set("", "", "", msg)
		return false
	}
	switch len(c.seen) {
	case 0:
	case 1:
		set("", c.seen[0], m.plan.Finder, "")
	default:
		set("", "", m.plan.Finder, fmt.Sprintf("%v: %d conversations in this folder could be this session's (%s)",
			finder.ErrAmbiguous, len(c.seen), strings.Join(c.seen, ", ")))
	}
	return false
}

func (c *conversationFinder) note(id string) {
	if !slices.Contains(c.seen, id) {
		c.seen = append(c.seen, id)
	}
}

// run looks up on findSchedule, then every findEvery, until an exact answer
// or stop closes.
func (c *conversationFinder) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	start := time.Now()
	next := func(i int) time.Duration {
		if i < len(findSchedule) {
			return time.Until(start.Add(findSchedule[i]))
		}
		return findEvery
	}
	for i := 0; ; i++ {
		t := time.NewTimer(next(i))
		select {
		case <-stop:
			t.Stop()
			return
		case <-t.C:
		}
		if c.lookup(false) {
			return
		}
	}
}

// atExit is the last lookup, after the run loop has stopped: what the agent
// wrote is there now. It waits at most exitLookupTimeout; a lookup still
// running then cannot record anything once the exit record is taken.
func (c *conversationFinder) atExit() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.lookup(true)
	}()
	select {
	case <-done:
	case <-time.After(exitLookupTimeout):
		c.m.log.Printf("conversation lookup at exit took over %s; not waiting", exitLookupTimeout)
	}
}
