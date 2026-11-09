package cli_test

import (
	"encoding/json"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startOnReboot starts a fake bridge with --restart on-reboot and waits for
// its URL (run --remote does).
func (h *harness) startOnReboot(name string, env ...string) {
	h.t.Helper()
	args := []string{"run", "fakebridge", "--remote", "--restart", "on-reboot", "--cwd", h.work, "--name", name,
		"--env", "FAKE_BRIDGE=1"}
	for _, e := range env {
		args = append(args, "--env", e)
	}
	h.ok(args...)
}

// simulateReboot SIGKILLs a session's monitor and its agent's whole process
// group, as a reboot would, and waits for the session to read lost.
func (h *harness) simulateReboot(name string) {
	h.t.Helper()
	v := h.inspect(name)
	if v.Status.Monitor == nil || v.Status.PGID <= 0 {
		h.t.Fatalf("%s: no monitor or pgid recorded: %+v", name, v)
	}
	_ = syscall.Kill(-v.Status.PGID, syscall.SIGKILL)
	_ = syscall.Kill(v.Status.Monitor.PID, syscall.SIGKILL)
	h.waitState(name, "lost", 10*time.Second)
	// The group is gone too (no orphans to block a restore).
	deadline := time.Now().Add(5 * time.Second)
	for h.inspect(name).Orphans > 0 {
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: orphans survived SIGKILL", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunRestartUsageErrors(t *testing.T) {
	h := newBridgeHarness(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"run", "fake", "--cwd", h.work, "--prompt", "hi", "--restart", "on-reboot"},
			"--restart on-reboot needs --remote"},
		{[]string{"run", "fakebridge", "--remote", "--cwd", h.work, "--restart", "always"},
			`unknown --restart "always" (want no or on-reboot)`},
	}
	for _, c := range cases {
		_, errOut, code := h.cmd(c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, stderr %q; want exit 2 with %q", c.args, code, errOut, c.want)
		}
	}
	if out := h.ok("ps", "--all", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("sessions left behind: %s", out)
	}
}

func TestRunRestartRecorded(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("br-pin")
	h.ok("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "br-nopin", "--env", "FAKE_BRIDGE=1")
	for name, want := range map[string]string{"br-pin": "on-reboot", "br-nopin": "no"} {
		var v struct {
			Session struct {
				Restart string `json:"restart"`
			} `json:"session"`
		}
		if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
			t.Fatal(err)
		}
		if v.Session.Restart != want {
			t.Errorf("%s: restart = %q, want %q", name, v.Session.Restart, want)
		}
	}
}

// TestPruneKeepsOnReboot: prune never removes an on-reboot session, whether
// it ended or is lost, while an ordinary ended bridge goes.
func TestPruneKeepsOnReboot(t *testing.T) {
	h := newBridgeHarness(t)
	h.startOnReboot("pin-ended")
	h.ok("stop", "pin-ended")
	h.waitState("pin-ended", "exited", 10*time.Second)
	h.startOnReboot("pin-lost")
	h.simulateReboot("pin-lost")
	h.ok("run", "fakebridge", "--remote", "--cwd", h.work, "--name", "plain", "--env", "FAKE_BRIDGE=1")
	h.ok("stop", "plain")
	h.waitState("plain", "exited", 10*time.Second)
	time.Sleep(10 * time.Millisecond)

	var out struct {
		Removed []string `json:"removed"`
		Kept    []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"kept"`
	}
	if err := json.Unmarshal([]byte(h.ok("prune", "--older-than", "1ms", "--json")), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Removed) != 1 || out.Removed[0] != "plain" {
		t.Errorf("removed = %v, want [plain]", out.Removed)
	}
	kept := map[string]string{}
	for _, k := range out.Kept {
		kept[k.Name] = k.Reason
	}
	if kept["pin-ended"] != "pinned" || kept["pin-lost"] != "lost" {
		t.Errorf("kept = %v, want pin-ended pinned and pin-lost lost", kept)
	}
	for _, n := range []string{"pin-ended", "pin-lost"} {
		h.inspect(n) // still there
	}
}
