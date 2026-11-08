package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

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

func TestWithOrphanScan(t *testing.T) {
	full := &session.View{Orphans: []proc.Identity{{PID: 1, StartTime: 1}}}
	fail := func(string, time.Duration) (*session.View, error) { return nil, errors.New("boom") }

	t.Run("retries once and succeeds", func(t *testing.T) {
		calls := 0
		load := func(string, time.Duration) (*session.View, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("transient")
			}
			return full, nil
		}
		if got := withOrphanScan(&session.View{}, "s", time.Second, load); got != full || calls != 2 {
			t.Fatalf("got %p after %d calls, want the full view after 2", got, calls)
		}
	})
	t.Run("gives up and marks the count unknown", func(t *testing.T) {
		cheap := &session.View{}
		got := withOrphanScan(cheap, "s", time.Second, fail)
		if got != cheap || !strings.Contains(got.OrphanErr, "boom") {
			t.Fatalf("OrphanErr = %q", got.OrphanErr)
		}
	})
	t.Run("keeps an earlier OrphanErr", func(t *testing.T) {
		cheap := &session.View{OrphanErr: "no readable plan"}
		if got := withOrphanScan(cheap, "s", time.Second, fail); got.OrphanErr != "no readable plan" {
			t.Fatalf("OrphanErr = %q", got.OrphanErr)
		}
	})
}

func TestInspectOutCarriesOrphanError(t *testing.T) {
	out := inspectOf(&session.View{OrphanErr: "scan failed"})
	if out.OrphanError != "scan failed" {
		t.Fatalf("OrphanError = %q", out.OrphanError)
	}
}
