package session

import (
	"errors"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

// noUnit is a unit checker for tests whose records never name a unit.
func noUnit(string) (bool, error) { return false, errors.New("unit checker called unexpectedly") }

// stubUnit replaces the package's unit checker for the test.
func stubUnit(t *testing.T, f func(string) (bool, error)) {
	t.Helper()
	old := unitCheck
	unitCheck = f
	t.Cleanup(func() { unitCheck = old })
}

func unitRecord(unit string) *LauncherRecord {
	return &LauncherRecord{SchemaVersion: SchemaVersion, Launcher: "systemd", Unit: unit, BootID: "boot-1"}
}

func TestResolveUnitOnly(t *testing.T) {
	const unit = "fastener-x-g1-abc.service"
	boom := errors.New("systemctl --user show: timed out")
	noReload := func() (Status, *LauncherRecord, *Plan, error) { return Status{}, nil, nil, errors.New("unused") }
	check := func(proc.Identity, string) (bool, error) {
		t.Fatal("process check called for a unit-only record")
		return false, nil
	}
	old := time.Now().Add(-time.Hour) // well past 2x handshake
	cases := []struct {
		name, state string
		unit        func(string) (bool, error)
		want        string
		wantErr     string
	}{
		{"active running", StateRunning, func(string) (bool, error) { return true, nil }, StateRunning, ""},
		{"active starting", StateStarting, func(string) (bool, error) { return true, nil }, StateStarting, ""},
		{"inactive running", StateRunning, func(string) (bool, error) { return false, nil }, StateLost, ""},
		{"inactive starting", StateStarting, func(string) (bool, error) { return false, nil }, StateLost, ""},
		{"unknown", StateRunning, func(string) (bool, error) { return false, boom }, StateRunning, boom.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked string
			v := &View{Session: Session{CreatedAt: old}, Status: Status{State: c.state}, Launcher: unitRecord(unit)}
			v.resolve(time.Second, time.Now(), check, func(u string) (bool, error) { asked = u; return c.unit(u) }, noReload)
			if asked != unit {
				t.Fatalf("unit checker asked about %q, want %q", asked, unit)
			}
			if v.State != c.want || v.LivenessError != c.wantErr {
				t.Fatalf("state %s, liveness error %q; want %s, %q", v.State, v.LivenessError, c.want, c.wantErr)
			}
		})
	}
}

func TestResolveUnitIgnoredWhenMonitorKnown(t *testing.T) {
	// With a monitor identity, the process check decides; the unit is not asked.
	id := proc.Identity{PID: 4242, StartTime: 1}
	rec := unitRecord("u.service")
	rec.Monitor = &id
	v := &View{Status: Status{State: StateRunning}, Launcher: rec}
	v.resolve(time.Second, time.Now(), func(proc.Identity, string) (bool, error) { return true, nil }, noUnit,
		func() (Status, *LauncherRecord, *Plan, error) { return Status{}, nil, nil, errors.New("unused") })
	if v.State != StateRunning {
		t.Fatalf("state %s", v.State)
	}
}

func TestLoadUnitOnly(t *testing.T) {
	setup(t)
	alive := map[string]bool{"up.service": true, "down.service": false}
	stubUnit(t, func(u string) (bool, error) {
		if u == "unknown.service" {
			return false, errors.New("bus unavailable")
		}
		return alive[u], nil
	})
	old := time.Now().Add(-time.Hour)
	writeSession(t, "up", old, Status{State: StateStarting}, unitRecord("up.service"))
	writeSession(t, "down", old, Status{State: StateStarting}, unitRecord("down.service"))
	writeSession(t, "unknown", old, Status{State: StateRunning}, unitRecord("unknown.service"))
	for name, want := range map[string]string{"up": StateStarting, "down": StateLost, "unknown": StateRunning} {
		v, err := Load(name, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != want {
			t.Errorf("%s: state %s, want %s", name, v.State, want)
		}
		if name == "unknown" && v.LivenessError == "" {
			t.Errorf("unknown: no liveness error recorded")
		}
	}
}

func TestFinalizeUnitOnly(t *testing.T) {
	setup(t)
	state := map[string]error{"up.service": nil, "unknown.service": errors.New("bus unavailable")}
	stubUnit(t, func(u string) (bool, error) {
		if err, ok := state[u]; ok {
			return err == nil, err
		}
		return false, nil
	})
	for _, u := range []string{"up.service", "unknown.service"} {
		d := writeSession(t, "s-"+u[:2], time.Now(), Status{State: StateStarting}, unitRecord(u))
		ok, err := Finalize(d, func(s *Status) { s.State = StateExited })
		if ok || !errors.Is(err, ErrMonitorAlive) {
			t.Fatalf("%s: ok=%v err=%v, want ErrMonitorAlive", u, ok, err)
		}
	}
	d := writeSession(t, "gone", time.Now(), Status{State: StateStarting}, unitRecord("gone.service"))
	if ok, err := Finalize(d, func(s *Status) { s.State = StateExited }); !ok || err != nil {
		t.Fatalf("collected unit: ok=%v err=%v", ok, err)
	}
}
