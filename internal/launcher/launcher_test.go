package launcher

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/session"
	"golang.org/x/sys/unix"
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

func stub[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func stubManager(t *testing.T, ok bool, err error, lingers bool, lerr error) {
	t.Helper()
	stub(t, &available, func() (bool, error) { return ok, err })
	stub(t, &linger, func() (bool, error) { return lingers, lerr })
}

func TestSelect(t *testing.T) {
	var warn strings.Builder
	stubManager(t, true, nil, true, nil)
	for pref, want := range map[string]string{"": "systemd", "auto": "systemd", "systemd": "systemd", "setsid": "setsid"} {
		if l, err := Select(pref, &warn); err != nil || l.Name() != want {
			t.Errorf("available, %q: %v %v, want %s", pref, l, err, want)
		}
	}
	if warn.Len() != 0 {
		t.Errorf("warned with linger on: %q", warn.String())
	}

	// No linger: auto falls back; explicit systemd works but warns.
	stubManager(t, true, nil, false, nil)
	if l, err := Select("auto", &warn); err != nil || l.Name() != "setsid" {
		t.Errorf("auto without linger: %v %v", l, err)
	}
	if warn.Len() != 0 {
		t.Errorf("auto warned: %q", warn.String())
	}
	if l, err := Select("systemd", &warn); err != nil || l.Name() != "systemd" || !strings.Contains(warn.String(), "stop when you log out") {
		t.Errorf("systemd without linger: %v %v, warning %q", l, err, warn.String())
	}
	warn.Reset()
	stubManager(t, true, nil, false, errors.New("loginctl: timed out"))
	if l, _ := Select("auto", &warn); l.Name() != "setsid" {
		t.Errorf("auto with unknown linger chose %s", l.Name())
	}
	if _, err := Select("systemd", &warn); err != nil || !strings.Contains(warn.String(), "loginctl: timed out") {
		t.Errorf("systemd with unknown linger: %v, warning %q", err, warn.String())
	}

	stubManager(t, false, errors.New("Failed to connect to bus"), true, nil)
	for pref, want := range map[string]string{"": "setsid", "auto": "setsid", "setsid": "setsid"} {
		if l, err := Select(pref, &warn); err != nil || l.Name() != want {
			t.Errorf("unavailable, %q: %v %v, want %s", pref, l, err, want)
		}
	}
	if _, err := Select("systemd", &warn); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("explicit systemd without a manager: %v", err)
	}
	if _, err := Select("bogus", &warn); err == nil {
		t.Error("unknown launcher accepted")
	}
}

func TestSystemdRunArgs(t *testing.T) {
	sess := session.Session{Name: "x", Grace: session.Duration(1500 * time.Millisecond)}
	args := systemdRunArgs("u.service", sess, "/s/monitor.log", "4096:524288", []string{"/bin/fastener", "_monitor", "/s/$HOME/x", "1"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--user", "--unit=u.service", "--collect", "KillMode=mixed",
		"TimeoutStopSec=32s", "LimitNOFILE=4096:524288", "StandardOutput=append:/s/monitor.log", "StandardError=append:/s/monitor.log",
		"-- /bin/fastener _monitor /s/$$HOME/x 1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "RuntimeMaxSec") {
		t.Errorf("RuntimeMaxSec without a max runtime: %s", joined)
	}
	sess.MaxRuntime = session.Duration(time.Minute)
	if joined := strings.Join(systemdRunArgs("u.service", sess, "/l", "", []string{"/f"}), " "); !strings.Contains(joined, "RuntimeMaxSec=92s") || strings.Contains(joined, "LimitNOFILE") {
		t.Errorf("RuntimeMaxSec / no limit: %s", joined)
	}
	sess.Grace = 0 // unset: the 10s default
	if joined := strings.Join(systemdRunArgs("u.service", sess, "/l", "", []string{"/f"}), " "); !strings.Contains(joined, "TimeoutStopSec=40s") {
		t.Errorf("default grace: %s", joined)
	}
}

func TestNofileLimit(t *testing.T) {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Skip(err)
	}
	stub(t, &childSoftNofile, func() (uint64, bool) { return 1024, true })
	got := nofileLimit()
	soft := lim.Cur
	if lim.Cur == lim.Max-1 {
		soft = 1024 // Go's raise is undone with the child's view
	}
	if want := rlimitString(soft) + ":" + rlimitString(lim.Max); got != want {
		t.Fatalf("nofileLimit = %q, want %q (getrlimit %d:%d)", got, want, lim.Cur, lim.Max)
	}
}

// A systemd-run that times out after the manager accepted the job must not
// leave the unit running unannounced.
func TestStartFailureAfterJobAccepted(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
	d, err := session.Create("x")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WriteJSON(d.SessionPath(), session.Session{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	stub(t, &runSystemdRun, func([]string) ([]byte, error) { return nil, errors.New("timed out after 30s") })
	var killed, reset []string
	stub(t, &resetFailedUnit, func(u string) error { reset = append(reset, u); return nil })

	// Not alive: the error is returned as is, nothing is killed.
	stub(t, &unitAlive, func(string) (bool, error) { return false, nil })
	stub(t, &killUnit, func(u string) error { killed = append(killed, u); return nil })
	if _, err := (Systemd{}).Start(d, 1); err == nil || !strings.Contains(err.Error(), "fastener-x-g1-") || len(killed) != 0 {
		t.Fatalf("dead unit: err %v, killed %v", err, killed)
	}

	// Alive (or unknown): killed, reset, and the error names the unit.
	for _, alive := range []func(string) (bool, error){
		func(string) (bool, error) { return true, nil },
		func(string) (bool, error) { return false, errors.New("bus timeout") },
	} {
		killed, reset = nil, nil
		stub(t, &unitAlive, alive)
		_, err := (Systemd{}).Start(d, 1)
		if err == nil || len(killed) != 1 || !strings.Contains(err.Error(), killed[0]) || len(reset) != 1 || reset[0] != killed[0] {
			t.Fatalf("alive unit: err %v, killed %v, reset %v", err, killed, reset)
		}
	}

	// The kill fails: a unit-only record, so the handshake and stop can manage it.
	stub(t, &unitAlive, func(string) (bool, error) { return true, nil })
	stub(t, &killUnit, func(string) error { return errors.New("access denied") })
	rec, err := (Systemd{}).Start(d, 1)
	if err != nil || rec.Launcher != "systemd" || !strings.HasPrefix(rec.Unit, "fastener-x-g1-") || rec.Monitor != nil {
		t.Fatalf("kill failed: rec %+v, err %v", rec, err)
	}
}

func TestUnitName(t *testing.T) {
	a, b := unitName("bd-1.x_y", 2), unitName("bd-1.x_y", 2)
	if a == b || !strings.HasPrefix(a, "fastener-bd-1.x_y-g2-") || !strings.HasSuffix(a, ".service") {
		t.Fatalf("%q %q", a, b)
	}
}

// TestSelectRestoreManagerStarting: at boot the user manager is still
// starting. Select refuses it (systemd) or falls back (auto); SelectRestore
// uses it.
func TestSelectRestoreManagerStarting(t *testing.T) {
	stubManager(t, false, errors.New("user manager is starting"), true, nil)
	stub(t, &availableBooting, func() (bool, error) { return true, nil })
	var warn strings.Builder
	if _, err := Select("systemd", &warn); err == nil {
		t.Error("Select(systemd) accepted a starting manager")
	}
	if l, _ := Select("auto", &warn); l.Name() != "setsid" {
		t.Errorf("Select(auto) = %s", l.Name())
	}
	for _, pref := range []string{"systemd", "auto", ""} {
		if l, err := SelectRestore(pref, &warn); err != nil || l.Name() != "systemd" {
			t.Errorf("SelectRestore(%q) = %v, %v; want systemd", pref, l, err)
		}
	}
	if l, err := SelectRestore("setsid", &warn); err != nil || l.Name() != "setsid" {
		t.Errorf("SelectRestore(setsid) = %v, %v", l, err)
	}
	stub(t, &availableBooting, func() (bool, error) { return false, errors.New("no manager") })
	if _, err := SelectRestore("systemd", &warn); err == nil {
		t.Error("SelectRestore(systemd) without a manager succeeded")
	}
}
