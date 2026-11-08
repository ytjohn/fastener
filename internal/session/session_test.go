package session

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(WriteJSON(d.SessionPath(), Session{SchemaVersion: 1, Name: name, CreatedAt: created}))
	must(WriteJSON(d.PlanPath(1), Plan{SchemaVersion: 1, Generation: 1, Marker: name + ":1:x"}))
	must(WriteJSON(d.StatusPath(), st))
	if l != nil {
		must(WriteJSON(d.LauncherPath(1), l))
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

func TestResolveRereadBeforeLost(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	exited := Status{State: StateExited, EndedBy: EndedByExit, Monitor: &dead, BootID: boot}
	v := &View{Status: Status{State: StateRunning, Monitor: &dead, BootID: boot}}
	v.resolve(time.Second, time.Now(), proc.Check, noUnit, func() (Status, *LauncherRecord, *Plan, error) { return exited, nil, nil, nil })
	if v.State != StateExited || v.Status.EndedBy != EndedByExit {
		t.Fatalf("state %s, status %+v", v.State, v.Status)
	}
	// unchanged on re-read: lost, with the fresh copy installed
	fresh := Status{State: StateRunning, Monitor: &dead, BootID: boot, Generation: 1}
	v = &View{Status: Status{State: StateRunning, Monitor: &dead, BootID: boot}}
	v.resolve(time.Second, time.Now(), proc.Check, noUnit, func() (Status, *LauncherRecord, *Plan, error) { return fresh, nil, nil, nil })
	if v.State != StateLost || v.Status.Generation != 1 {
		t.Fatalf("state %s, status %+v", v.State, v.Status)
	}
	// a different, live monitor appears
	self, _ := proc.Self()
	live := Status{State: StateRunning, Monitor: &self, BootID: boot}
	v = &View{Status: Status{State: StateRunning, Monitor: &dead, BootID: boot}}
	v.resolve(time.Second, time.Now(), proc.Check, noUnit, func() (Status, *LauncherRecord, *Plan, error) { return live, nil, nil, nil })
	if v.State != StateRunning {
		t.Fatalf("state %s", v.State)
	}
}

func TestFinalizeRefusesLiveMonitor(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	self, _ := proc.Self()
	d := writeSession(t, "live", time.Now(), Status{State: StateRunning, Monitor: &self, BootID: boot}, nil)
	ok, err := Finalize(d, func(s *Status) { s.State = StateExited })
	if ok || !errors.Is(err, ErrMonitorAlive) {
		t.Fatalf("status.json monitor: ok=%v err=%v", ok, err)
	}
	d2 := writeSession(t, "live2", time.Now(), Status{State: StateStarting}, &LauncherRecord{Monitor: &self, BootID: boot})
	ok, err = Finalize(d2, func(s *Status) { s.State = StateExited })
	if ok || !errors.Is(err, ErrMonitorAlive) {
		t.Fatalf("launcher monitor: ok=%v err=%v", ok, err)
	}
}

func TestFinalizeKeepsExitedContent(t *testing.T) {
	setup(t)
	d := writeSession(t, "kept", time.Now(), Status{SchemaVersion: 1, State: StateExited, EndedBy: EndedByLimit}, nil)
	ok, err := Finalize(d, func(s *Status) { s.State = StateExited; s.EndedBy = EndedBySignal })
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var st Status
	if err := ReadJSON(d.StatusPath(), &st); err != nil || st.EndedBy != EndedByLimit {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestFinalizeKeepsNewerSchema(t *testing.T) {
	setup(t)
	d := writeSession(t, "schema", time.Now(), Status{SchemaVersion: 99, State: StateRunning}, nil)
	if ok, err := Finalize(d, func(s *Status) { s.State = StateExited }); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var st Status
	if err := ReadJSON(d.StatusPath(), &st); err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != 99 {
		t.Fatalf("schema_version = %d", st.SchemaVersion)
	}
}

func TestLoadFullFindsOrphans(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), proc.MarkerEnv+"=orph:1:x")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	writeSession(t, "orph", time.Now(), Status{State: StateRunning, Monitor: &dead, BootID: boot, PGID: cmd.Process.Pid}, nil)
	v, err := Load("orph", time.Second)
	if err != nil || v.State != StateLost || len(v.Orphans) != 0 {
		t.Fatalf("Load: %v state=%s orphans=%v", err, v.State, v.Orphans)
	}
	v, err = LoadFull("orph", time.Second)
	if err != nil || len(v.Orphans) != 1 || v.Orphans[0].PID != cmd.Process.Pid {
		t.Fatalf("LoadFull: %v orphans=%v", err, v.Orphans)
	}
}

func TestHalfCreatedSessions(t *testing.T) {
	setup(t)
	d, _ := Create("nostatus")
	WriteJSON(d.SessionPath(), Session{Name: "nostatus", CreatedAt: time.Now().Add(-time.Minute)})
	v, err := Load("nostatus", time.Second)
	if err != nil || v.State != StateLost {
		t.Fatalf("no status.json: %v %v", err, v)
	}
	d, _ = Create("nosession")
	v, err = Load("nosession", time.Hour)
	if err != nil || v.State != StateStarting || v.Session.Name != "nosession" || v.Session.CreatedAt.IsZero() {
		t.Fatalf("no session.json: %v %+v", err, v)
	}
	os.WriteFile(d.SessionPath(), []byte("{bad"), 0o600)
	if _, err := Load("nosession", time.Hour); err == nil {
		t.Fatal("corrupt session.json accepted")
	}
}

func TestDurationNullAndNumber(t *testing.T) {
	var s Session
	if err := json.Unmarshal([]byte(`{"grace":null,"max_runtime":1500000000}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Grace != 0 || time.Duration(s.MaxRuntime) != 1500*time.Millisecond {
		t.Fatalf("%+v", s)
	}
}

func TestFinalizeMissingStatus(t *testing.T) {
	setup(t)
	d, _ := Create("nostat")
	ok, err := Finalize(d, func(s *Status) { s.State = StateExited; s.EndedBy = EndedByLaunchFailed })
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var st Status
	if err := ReadJSON(d.StatusPath(), &st); err != nil || st.State != StateExited {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestResolveUnknownStateNotLost(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	v := &View{Status: Status{State: StateRunning, Monitor: &dead, BootID: boot}}
	v.resolve(time.Second, time.Now(), proc.Check, noUnit, func() (Status, *LauncherRecord, *Plan, error) {
		return Status{State: "future-state"}, nil, nil, nil
	})
	if v.State != "future-state" {
		t.Fatalf("state %s", v.State)
	}
}

func TestResolveReloadsPlanOnGenerationChange(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	d := writeSession(t, "gen", time.Now(), Status{State: StateRunning, Monitor: &dead, BootID: boot}, nil)
	if err := WriteJSON(d.PlanPath(2), Plan{Generation: 2, Marker: "gen:2:y"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(d.StatusPath(), Status{State: StateRunning, Generation: 2, Monitor: &dead, BootID: boot}); err != nil {
		t.Fatal(err)
	}
	v, err := Load("gen", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v.Plan == nil || v.Plan.Generation != 2 {
		t.Fatalf("plan %+v", v.Plan)
	}
}

func TestRootAbsoluteForRelativeStateDir(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", "rel/state")
	if r := Root(); !filepath.IsAbs(r) || !strings.HasSuffix(r, "rel/state") {
		t.Fatalf("Root() = %q", r)
	}
}

func TestLoadWarnsOnNewerSchemaVersion(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	d := writeSession(t, "future", time.Now(), Status{SchemaVersion: SchemaVersion + 1, State: StateExited, EndedBy: EndedByExit, Monitor: &dead, BootID: boot},
		&LauncherRecord{SchemaVersion: SchemaVersion + 2, Launcher: "setsid", Monitor: &dead, BootID: boot})
	if err := WriteJSON(d.SessionPath(), Session{SchemaVersion: SchemaVersion + 3, Name: "future", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(d.PlanPath(1), Plan{SchemaVersion: SchemaVersion + 4, Generation: 1, Marker: "future:1:x"}); err != nil {
		t.Fatal(err)
	}
	v, err := Load("future", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"session.json has schema_version 4; this fastener understands up to 1, so some fields may be ignored",
		"status.json has schema_version 2; this fastener understands up to 1, so some fields may be ignored",
		"generations/1.json has schema_version 5; this fastener understands up to 1, so some fields may be ignored",
		"generations/1.launcher.json has schema_version 3; this fastener understands up to 1, so some fields may be ignored",
	}
	if strings.Join(v.Warnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings:\n%s\nwant:\n%s", strings.Join(v.Warnings, "\n"), strings.Join(want, "\n"))
	}
	if v.State != StateExited {
		t.Fatalf("state %s", v.State)
	}
}

func TestLoadNoWarningsAtCurrentOrOlderSchema(t *testing.T) {
	setup(t)
	boot, _ := proc.BootID()
	dead := deadIdentity(t)
	writeSession(t, "now", time.Now(), Status{SchemaVersion: SchemaVersion, State: StateExited, Monitor: &dead, BootID: boot},
		&LauncherRecord{SchemaVersion: SchemaVersion, Launcher: "setsid", Monitor: &dead, BootID: boot})
	v, err := Load("now", time.Second)
	if err != nil || len(v.Warnings) != 0 {
		t.Fatalf("%v %v", v.Warnings, err)
	}
}

func TestResolveRecordsUnknownLiveness(t *testing.T) {
	boot := "boot-1"
	id := proc.Identity{PID: 4242, StartTime: 1}
	boom := errors.New("read /proc/4242/stat: permission denied")
	unknown := func(proc.Identity, string) (bool, error) { return false, boom }
	gone := func(proc.Identity, string) (bool, error) { return false, nil }
	alive := func(proc.Identity, string) (bool, error) { return true, nil }
	noReload := func() (Status, *LauncherRecord, *Plan, error) { return Status{}, nil, nil, errors.New("unused") }
	mk := func() *View { return &View{Status: Status{State: StateRunning, Monitor: &id, BootID: boot}} }

	v := mk()
	v.resolve(time.Second, time.Now(), unknown, noUnit, noReload)
	if v.State != StateRunning || v.LivenessError != boom.Error() {
		t.Fatalf("unknown: state %s, err %q", v.State, v.LivenessError)
	}

	v = mk()
	v.resolve(time.Second, time.Now(), alive, noUnit, noReload)
	if v.State != StateRunning || v.LivenessError != "" {
		t.Fatalf("alive: state %s, err %q", v.State, v.LivenessError)
	}

	v = mk()
	v.resolve(time.Second, time.Now(), gone, noUnit, noReload)
	if v.State != StateLost || v.LivenessError != "" {
		t.Fatalf("gone: state %s, err %q", v.State, v.LivenessError)
	}

	// An exited session never consults the checker.
	v = &View{Status: Status{State: StateExited, Monitor: &id, BootID: boot}}
	v.resolve(time.Second, time.Now(), func(proc.Identity, string) (bool, error) {
		t.Fatal("checker called for exited session")
		return false, nil
	}, noUnit, noReload)
	if v.State != StateExited || v.LivenessError != "" {
		t.Fatalf("exited: state %s, err %q", v.State, v.LivenessError)
	}

	// Unknown liveness is not kept if the re-read shows the session ended.
	v = mk()
	exited := Status{State: StateExited, Monitor: &id, BootID: boot}
	v.resolve(time.Second, time.Now(), gone, noUnit, func() (Status, *LauncherRecord, *Plan, error) { return exited, nil, nil, nil })
	if v.State != StateExited || v.LivenessError != "" {
		t.Fatalf("reloaded: state %s, err %q", v.State, v.LivenessError)
	}
}
