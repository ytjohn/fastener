package cli

import (
	"testing"

	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/session"
)

func TestHandshakeLostAction(t *testing.T) {
	agent := &proc.Identity{PID: 42, StartTime: 1}
	cases := []struct {
		name string
		v    session.View
		want bool
	}{
		{"never started", session.View{Status: session.Status{State: session.StateStarting}}, true},
		{"agent spawned", session.View{Status: session.Status{State: session.StateRunning, Agent: agent}}, false},
		{"agent recorded, still starting", session.View{Status: session.Status{State: session.StateStarting, Agent: agent}}, true},
		{"marked process alive", session.View{Status: session.Status{State: session.StateStarting}, Orphans: []proc.Identity{*agent}}, false},
	}
	for _, c := range cases {
		if got := handshakeLostAction(&c.v); got != c.want {
			t.Errorf("%s: finalize = %v, want %v", c.name, got, c.want)
		}
	}
}
