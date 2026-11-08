package session

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

func setup(t *testing.T) {
	t.Helper()
	t.Setenv("FASTENER_STATE_DIR", t.TempDir())
}

func TestCreateUniqueAndNames(t *testing.T) {
	setup(t)
	if _, err := Create("dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := Create("dev"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate create: %v", err)
	}
	if _, err := Create("Bad Name"); err == nil {
		t.Fatal("invalid name accepted")
	}
	names, _ := Names()
	if len(names) != 1 || names[0] != "dev" {
		t.Fatalf("Names = %v", names)
	}
	if n := GenerateName("claude"); !ValidName(n) || len(n) != len("claude-")+6 {
		t.Fatalf("GenerateName = %q", n)
	}
}

func TestDurationJSON(t *testing.T) {
	setup(t)
	d, _ := Create("d")
	in := Session{Name: "d", Grace: Duration(10 * time.Second), MaxRuntime: Duration(40 * time.Minute)}
	if err := WriteJSON(d.SessionPath(), in); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(d.SessionPath())
	if !strings.Contains(string(b), `"grace": "10s"`) || !strings.Contains(string(b), `"max_runtime": "40m0s"`) {
		t.Fatalf("json = %s", b)
	}
	var out Session
	if err := ReadJSON(d.SessionPath(), &out); err != nil || out.MaxRuntime != in.MaxRuntime {
		t.Fatalf("round trip: %+v %v", out, err)
	}
}

// writeSession writes a minimal session for view tests.
func writeSession(t *testing.T, name string, created time.Time, st Status, l *LauncherRecord) Dir {
	t.Helper()
	d, err := Create(name)
	if err != nil {
		t.Fatal(err)
	}
	WriteJSON(d.SessionPath(), Session{SchemaVersion: 1, Name: name, CreatedAt: created})
	WriteJSON(d.PlanPath(1), Plan{SchemaVersion: 1, Generation: 1, Marker: name + ":1:x"})
	WriteJSON(d.StatusPath(), st)
	if l != nil {
		WriteJSON(d.LauncherPath(1), l)
	}
	return d
}

func deadIdentity(t *testing.T) proc.Identity {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	id, _ := proc.Of(cmd.Process.Pid)
	if id.PID == 0 {
		id = proc.Identity{PID: cmd.Process.Pid, StartTime: 1}
	}
	cmd.Wait()
	return id
}

func TestViewLostStates(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	self, _ := proc.Self()
	dead := deadIdentity(t)
	hs := 5 * time.Second
	now := time.Now().UTC()

	writeSession(t, "running-alive", now, Status{State: StateRunning, Monitor: &self, BootID: boot}, nil)
	writeSession(t, "running-dead", now, Status{State: StateRunning, Monitor: &dead, BootID: boot}, nil)
	writeSession(t, "starting-launcher-dead", now, Status{State: StateStarting},
		&LauncherRecord{Launcher: "setsid", Monitor: &dead, BootID: boot})
	writeSession(t, "starting-young-norecord", now, Status{State: StateStarting}, nil)
	writeSession(t, "starting-old-norecord", now.Add(-time.Minute), Status{State: StateStarting}, nil)
	writeSession(t, "exited", now, Status{State: StateExited, EndedBy: EndedByExit}, nil)

	want := map[string]string{
		"running-alive":           StateRunning,
		"running-dead":            StateLost,
		"starting-launcher-dead":  StateLost,
		"starting-young-norecord": StateStarting,
		"starting-old-norecord":   StateLost,
		"exited":                  StateExited,
	}
	for name, w := range want {
		v, err := Load(name, hs)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != w {
			t.Errorf("%s: state %s, want %s", name, v.State, w)
		}
	}
	if _, err := Load("missing", hs); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestFinalizeNeverOverwritesExited(t *testing.T) {
	setup(t)
	d := writeSession(t, "f", time.Now(), Status{State: StateRunning}, nil)
	var wrote int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := Finalize(d, func(s *Status) { s.State = StateExited; s.EndedBy = []string{"stop", "signal"}[i] })
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wrote++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wrote != 1 {
		t.Fatalf("finalize wrote %d times, want exactly 1", wrote)
	}
}

func TestNoStateDir(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "relative")
	t.Setenv("HOME", "")
	if r := Root(); r != "" {
		t.Fatalf("Root = %q, want empty", r)
	}
	want := "no state directory: set HOME or FASTENER_STATE_DIR"
	if _, err := Create("x"); err == nil || err.Error() != want {
		t.Fatalf("Create: %v", err)
	}
	if _, err := Names(); err == nil || err.Error() != want {
		t.Fatalf("Names: %v", err)
	}
	if SessionsDir() != "" || DirFor("x") != "" {
		t.Fatalf("relative path produced: %q %q", SessionsDir(), DirFor("x"))
	}
	if _, err := Load("x", time.Second); !errors.Is(err, ErrNoStateDir) {
		t.Fatalf("Load: %v", err)
	}
}

func TestRelativeXDGStateIgnored(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "relative")
	t.Setenv("HOME", "/home/x")
	if r := Root(); r != "/home/x/.local/state/fastener" {
		t.Fatalf("Root = %q", r)
	}
}
