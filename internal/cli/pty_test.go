package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/attach"
	"github.com/ytjohn/fastener/internal/launcher"
	"github.com/ytjohn/fastener/internal/session"
)

// Until `run --interactive` exists (#52), these tests write an interactive
// session's files themselves and start the real `fastener _monitor` the way
// each launcher does, then drive it through the CLI and the attach socket.

// startPTYSession writes session pty1 with a pty plan for the fake agent
// and starts its monitor with launch ("setsid" or "systemd").
func startPTYSession(t *testing.T, h *harness, launch string) session.Dir {
	t.Helper()
	// Not session.Create: it reads FASTENER_STATE_DIR, and t.Setenv is not
	// allowed in the parallel systemd tests.
	d := session.Dir(filepath.Join(h.state, "sessions", "pty1"))
	if err := os.MkdirAll(filepath.Join(string(d), "generations"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, err := range []error{
		session.WriteJSON(d.SessionPath(), session.Session{
			SchemaVersion: 1, Name: "pty1", CreatedAt: now, Kind: "interactive", Backend: "fake", Cwd: h.work,
			Grace: session.Duration(2 * time.Second), Restart: "no",
			EnvBase: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, Env: []string{"FAKE_PTY=1"},
		}),
		session.WriteJSON(d.PlanPath(1), session.Plan{
			SchemaVersion: 1, Generation: 1, Argv: []string{fakeBin}, PromptMode: "none", Parser: "none",
			PTY: &session.PTYPlan{Rows: 33, Cols: 101}, Marker: session.NewMarker("pty1", 1), PlannedAt: now,
		}),
		session.WriteJSON(d.StatusPath(), session.Status{SchemaVersion: 1, State: session.StateStarting}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// The real launchers, pointed at the fastener binary (the test binary
	// is not one).
	var l launcher.Launcher = launcher.Setsid{Exe: fastenerBin}
	if launch == "systemd" {
		l = launcher.Systemd{Exe: fastenerBin}
	}
	rec, err := l.Start(d, 1)
	if err != nil {
		t.Fatalf("%s launcher: %v", launch, err)
	}
	if err := session.WriteJSON(d.LauncherPath(1), rec); err != nil {
		t.Fatal(err)
	}
	if rec.Unit != "" {
		t.Cleanup(func() { waitUnitGone(t, rec.Unit, 45*time.Second) })
	}
	return d
}

type ptyInspect struct {
	State string `json:"state"`
	Plan  struct {
		PTY *struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		} `json:"pty"`
	} `json:"plan"`
	Status struct {
		AttachSocket    string `json:"attach_socket"`
		AttachedClients *int   `json:"attached_clients"`
		EndedBy         string `json:"ended_by"`
		ExitCode        *int   `json:"exit_code"`
	} `json:"status"`
}

type ptyPsRow struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	Interactive     bool   `json:"interactive"`
	AttachedClients *int   `json:"attached_clients"`
}

func inspectPTY(t *testing.T, h *harness) ptyInspect {
	t.Helper()
	var v ptyInspect
	if err := json.Unmarshal([]byte(h.ok("inspect", "pty1", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func psPTY(t *testing.T, h *harness) ptyPsRow {
	t.Helper()
	var rows []ptyPsRow
	if err := json.Unmarshal([]byte(h.ok("ps", "--json")), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("ps --json: %v %+v", err, rows)
	}
	return rows[0]
}

func poll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func recvText(t *testing.T, c *attach.Client, want string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	var got []byte
	for !bytes.Contains(got, []byte(want)) {
		m, err := c.Recv()
		if err != nil {
			t.Fatalf("waiting for %q (have %q): %v", want, got, err)
		}
		got = append(got, m.Data...)
	}
}

func testPTYSession(t *testing.T, h *harness, launch string) {
	d := startPTYSession(t, h, launch)
	poll(t, "running", func() bool { return inspectPTY(t, h).State == session.StateRunning })
	v := inspectPTY(t, h)
	if v.Plan.PTY == nil || v.Plan.PTY.Rows != 33 || v.Status.AttachSocket == "" {
		t.Fatalf("inspect: %+v", v)
	}
	if r := psPTY(t, h); !r.Interactive || r.AttachedClients == nil || *r.AttachedClients != 0 {
		t.Fatalf("ps before attach: %+v", r)
	}
	c, err := attach.Dial(v.Status.AttachSocket, attach.Options{Size: attach.Size{Rows: 40, Cols: 90}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	recvText(t, c, "winch=40x90")
	poll(t, "ps attached_clients=1", func() bool {
		r := psPTY(t, h)
		return r.AttachedClients != nil && *r.AttachedClients == 1
	})
	if err := c.Send([]byte("hi there\n")); err != nil {
		t.Fatal(err)
	}
	recvText(t, c, "echo:hi there")
	if out := h.ok("logs", "pty1"); !strings.Contains(out, "tty=true ctty=true size=33x101") || !strings.Contains(out, "echo:hi there") {
		t.Fatalf("logs:\n%s", out)
	}

	h.ok("stop", "pty1")
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var exit *attach.ExitInfo
	for exit == nil {
		m, err := c.Recv()
		if err != nil {
			t.Fatalf("waiting for exit: %v", err)
		}
		exit = m.Exit
	}
	if exit.EndedBy != session.EndedByStop {
		t.Fatalf("exit frame %+v", exit)
	}
	poll(t, "exited", func() bool { return inspectPTY(t, h).State == session.StateExited })
	v = inspectPTY(t, h)
	if v.Status.EndedBy != session.EndedByStop || v.Status.AttachSocket != "" || v.Status.AttachedClients != nil {
		t.Fatalf("final inspect: %+v", v)
	}
	if r := psPTY(t, h); !r.Interactive || r.AttachedClients != nil {
		t.Fatalf("ps after exit: %+v", r)
	}
	p, _ := attach.SocketPath(string(d))
	if _, err := os.Lstat(p); err == nil {
		t.Fatalf("%s left behind", p)
	}
}

func TestPTYSessionSetsid(t *testing.T) { testPTYSession(t, newHarness(t), "setsid") }

func TestPTYSessionSystemd(t *testing.T) {
	testPTYSession(t, newSystemdHarness(t, "systemd"), "systemd")
}
