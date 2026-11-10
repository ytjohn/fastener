package monitor

import (
	"errors"
	"time"

	"github.com/ytjohn/fastener/internal/finder"
	"github.com/ytjohn/fastener/internal/session"
)

// findSchedule is when, after the spawn, the monitor asks the plan's finder
// for the conversation; then hourly while the agent runs, and once more as
// it exits. Agents write their conversation at the first message, which may
// come much later than the start; each lookup is a directory listing or a
// stat, so even the hourly ones cost nothing. It stops once found.
var findSchedule = []time.Duration{
	2 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
	5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
}

var findEvery = time.Hour

// conversationFinder resolves an interactive generation's conversation_id
// with the plan's finder and records it through the monitor's status.
type conversationFinder struct {
	m    *monitor
	find finder.Finder
	q    finder.Query
	done bool // found: no more lookups (only touched by the goroutine, then by exit)
}

func (m *monitor) newConversationFinder(env []string, started time.Time) *conversationFinder {
	if m.plan.Finder == "" {
		return nil
	}
	f, ok := finder.Get(m.plan.Finder)
	if !ok {
		m.log.Printf("unknown finder %q; conversation_id will not be found", m.plan.Finder)
		return nil
	}
	return &conversationFinder{m: m, find: f, q: finder.Query{
		Cwd: m.sess.Cwd, StartedAt: started, Env: env, SessionID: m.plan.SessionID,
	}}
}

// lookup asks the finder once and records the outcome; it reports whether
// the conversation was found.
func (c *conversationFinder) lookup() bool {
	if c.done {
		return true
	}
	id, err := c.find(c.q)
	source := c.m.plan.Finder
	if c.q.SessionID != "" {
		source = "session_id"
	}
	m := c.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.final || m.st.State == session.StateExited {
		return true
	}
	switch {
	case err == nil && id != "":
		if m.st.ConversationID != id || m.st.ConversationSource != source || m.st.ConversationError != "" {
			m.st.ConversationID, m.st.ConversationSource, m.st.ConversationError = id, source, ""
			m.dirty = true
		}
		c.done = true
	case err != nil:
		msg := err.Error()
		if !errors.Is(err, finder.ErrNotFound) && !errors.Is(err, finder.ErrAmbiguous) {
			msg = "finder " + c.m.plan.Finder + ": " + msg
		}
		if m.st.ConversationError != msg {
			m.st.ConversationError = msg
			m.dirty = true
		}
	}
	return c.done
}

// run looks up on findSchedule, then hourly, until found or stop closes.
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
		if c.lookup() {
			return
		}
	}
}
