package launcher

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

func TestMonitorArgsAbsolute(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", "rel/state")
	d, args, err := monitorArgs(session.DirFor("x"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(string(d)) || args[0] != "_monitor" || args[1] != string(d) || args[2] != "2" {
		t.Fatalf("dir=%q args=%v", d, args)
	}
}

func stubAvailable(t *testing.T, ok bool, err error) {
	t.Helper()
	old := available
	available = func() (bool, error) { return ok, err }
	t.Cleanup(func() { available = old })
}

func TestSelect(t *testing.T) {
	stubAvailable(t, true, nil)
	for pref, want := range map[string]string{"": "systemd", "auto": "systemd", "systemd": "systemd", "setsid": "setsid"} {
		if l, err := Select(pref); err != nil || l.Name() != want {
			t.Errorf("available, %q: %v %v, want %s", pref, l, err, want)
		}
	}
	stubAvailable(t, false, errors.New("Failed to connect to bus"))
	for pref, want := range map[string]string{"": "setsid", "auto": "setsid", "setsid": "setsid"} {
		if l, err := Select(pref); err != nil || l.Name() != want {
			t.Errorf("unavailable, %q: %v %v, want %s", pref, l, err, want)
		}
	}
	if _, err := Select("systemd"); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("explicit systemd without a manager: %v", err)
	}
	if _, err := Select("bogus"); err == nil {
		t.Error("unknown launcher accepted")
	}
}

func TestSystemdRunArgs(t *testing.T) {
	sess := session.Session{Name: "x", Grace: session.Duration(1500 * time.Millisecond)}
	args := systemdRunArgs("u.service", sess, "/s/monitor.log", []string{"/bin/fastener", "_monitor", "/s/$HOME/x", "1"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--user", "--unit=u.service", "--collect", "KillMode=control-group",
		"TimeoutStopSec=32s", "StandardOutput=append:/s/monitor.log", "StandardError=append:/s/monitor.log",
		"-- /bin/fastener _monitor /s/$$HOME/x 1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "RuntimeMaxSec") {
		t.Errorf("RuntimeMaxSec without a max runtime: %s", joined)
	}
	sess.MaxRuntime = session.Duration(time.Minute)
	if joined := strings.Join(systemdRunArgs("u.service", sess, "/l", []string{"/f"}), " "); !strings.Contains(joined, "RuntimeMaxSec=92s") {
		t.Errorf("RuntimeMaxSec: %s", joined)
	}
	sess.Grace = 0 // unset: the 10s default
	if joined := strings.Join(systemdRunArgs("u.service", sess, "/l", []string{"/f"}), " "); !strings.Contains(joined, "TimeoutStopSec=40s") {
		t.Errorf("default grace: %s", joined)
	}
}

func TestUnitName(t *testing.T) {
	a, b := unitName("bd-1.x_y", 2), unitName("bd-1.x_y", 2)
	if a == b || !strings.HasPrefix(a, "fastener-bd-1.x_y-g2-") || !strings.HasSuffix(a, ".service") {
		t.Fatalf("%q %q", a, b)
	}
}
