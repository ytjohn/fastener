package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// backdate rewrites a session's ended_at (when it has one) and created_at.
func (h *harness) backdate(name string, age time.Duration) {
	h.t.Helper()
	old := time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
	edit := func(file string, key string, onlyIfPresent bool) {
		p := filepath.Join(h.state, "sessions", name, file)
		b, err := os.ReadFile(p)
		if err != nil {
			h.t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			h.t.Fatal(err)
		}
		if _, ok := m[key]; ok || !onlyIfPresent {
			m[key] = old
		}
		b, _ = json.Marshal(m)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	edit("status.json", "ended_at", true)
	edit("session.json", "created_at", false)
}

func (h *harness) exists(name string) bool {
	_, err := os.Stat(filepath.Join(h.state, "sessions", name))
	return err == nil
}

func (h *harness) stamp() string { return filepath.Join(h.state, ".last-prune") }

type pruneJSON struct {
	Removed []string `json:"removed"`
	Kept    []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"kept"`
}

func (h *harness) doneSession(name string, age time.Duration, env ...string) {
	h.t.Helper()
	// A fresh stamp keeps opportunistic pruning out of the way while the
	// fixture is built; tests that want it remove the stamp first.
	os.MkdirAll(h.state, 0o700)
	os.WriteFile(h.stamp(), []byte("x\n"), 0o600)
	h.start(name, env)
	h.ok("wait", name, "--timeout", "10s")
	h.backdate(name, age)
}

func TestPruneRemovesOldKeepsRecent(t *testing.T) {
	h := newHarness(t)
	h.doneSession("old", 200*time.Hour)
	h.doneSession("recent", time.Hour)
	out := h.ok("prune")
	if !strings.Contains(out, "old") || h.exists("old") || !h.exists("recent") {
		t.Fatalf("prune output %q; old exists=%v recent exists=%v", out, h.exists("old"), h.exists("recent"))
	}
}

func TestPruneOlderThanAndDryRunAndJSON(t *testing.T) {
	h := newHarness(t)
	h.doneSession("a", 3*time.Hour)
	h.doneSession("b", 10*time.Minute)
	var r pruneJSON
	if err := json.Unmarshal([]byte(h.ok("prune", "--older-than", "1h", "--dry-run", "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Removed) != 1 || r.Removed[0] != "a" || !h.exists("a") {
		t.Fatalf("dry run: %+v, a exists=%v", r, h.exists("a"))
	}
	if out := h.ok("prune", "--older-than", "1h", "--dry-run"); !strings.Contains(out, "would remove a") {
		t.Fatalf("dry-run text %q", out)
	}
	if err := json.Unmarshal([]byte(h.ok("prune", "--older-than", "1h", "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Removed) != 1 || h.exists("a") || !h.exists("b") {
		t.Fatalf("prune: %+v", r)
	}
	// Nothing to do: arrays, never null.
	if out := h.ok("prune", "--older-than", "1h", "--json"); !strings.Contains(out, `"removed": []`) || !strings.Contains(out, `"kept": []`) {
		t.Fatalf("empty result %q", out)
	}
}

func TestPruneBadOlderThan(t *testing.T) {
	h := newHarness(t)
	for _, a := range []string{"0", "0s", "-1h", "soon"} {
		if _, _, code := h.cmd("prune", "--older-than", a); code != 2 {
			t.Errorf("--older-than %q: exit %d, want 2", a, code)
		}
	}
	if _, _, code := h.cmd("prune", "extra"); code != 2 {
		t.Errorf("positional arg: exit %d, want 2", code)
	}
}

func TestPruneKeepsLost(t *testing.T) {
	h := newHarness(t)
	h.start("lostone", []string{"FAKE_SLEEP=60s"})
	h.waitState("lostone", "running", 5*time.Second)
	v := h.inspect("lostone")
	// Kill the monitor so the session reads as lost (harness cleanup reaps the agent).
	pid := v.Status.Monitor.PID
	syscall.Kill(pid, syscall.SIGKILL)
	h.waitState("lostone", "lost", 10*time.Second)
	h.backdate("lostone", 500*time.Hour)
	var r pruneJSON
	if err := json.Unmarshal([]byte(h.ok("prune", "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Removed) != 0 || len(r.Kept) != 1 || r.Kept[0].Name != "lostone" || r.Kept[0].Reason != "lost" || !h.exists("lostone") {
		t.Fatalf("lost session: %+v", r)
	}
}

func TestPruneKeepsExitedWithOrphans(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process scan test is Linux-specific")
	}
	h := newHarness(t)
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	h.start("orph", []string{"FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile}, "--grace", "1s")
	h.ok("wait", "orph", "--timeout", "10s")
	waitPid(t, pidfile)
	h.backdate("orph", 500*time.Hour)
	var r pruneJSON
	if err := json.Unmarshal([]byte(h.ok("prune", "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Removed) != 0 || len(r.Kept) != 1 || r.Kept[0].Reason != "orphans" || !h.exists("orph") {
		t.Fatalf("orphaned session: %+v", r)
	}
	// Once the descendants are cleaned up it becomes prunable.
	h.ok("stop", "orph", "--grace", "1s")
	h.ok("prune")
	if h.exists("orph") {
		t.Fatal("session still present after its orphans were stopped")
	}
}

func TestPsAutoPrunes(t *testing.T) {
	h := newHarness(t)
	h.doneSession("old", 200*time.Hour)
	h.doneSession("recent", time.Hour)
	os.Remove(h.stamp())
	h.ok("ps")
	if h.exists("old") || !h.exists("recent") {
		t.Fatalf("old exists=%v recent exists=%v", h.exists("old"), h.exists("recent"))
	}
	if fi, err := os.Stat(h.stamp()); err != nil || fi.Size() == 0 {
		t.Fatalf("stamp not written: %v", err)
	}
}

func TestAutoPruneThrottledByFreshStamp(t *testing.T) {
	h := newHarness(t)
	h.doneSession("old", 200*time.Hour)
	os.Remove(h.stamp())
	os.WriteFile(h.stamp(), []byte("x\n"), 0o600)
	h.ok("ps")
	if !h.exists("old") {
		t.Fatal("pruned despite a fresh stamp")
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(h.stamp(), past, past)
	h.ok("ps")
	if h.exists("old") {
		t.Fatal("stale stamp did not trigger a prune")
	}
}

func TestRunAutoPrunes(t *testing.T) {
	h := newHarness(t)
	h.doneSession("old", 200*time.Hour)
	os.Remove(h.stamp())
	h.start("fresh", nil)
	if h.exists("old") {
		t.Fatal("run did not prune")
	}
}

func TestAutoPruneDisabledByConfig(t *testing.T) {
	h := newHarness(t)
	os.WriteFile(filepath.Join(h.config, "config.toml"), []byte("auto_prune = false\n"), 0o600)
	h.doneSession("old", 200*time.Hour)
	os.Remove(h.stamp())
	h.ok("ps")
	if !h.exists("old") {
		t.Fatal("auto_prune = false still pruned")
	}
	if _, err := os.Stat(h.stamp()); err == nil {
		t.Fatal("stamp written while disabled")
	}
	h.ok("prune") // on-demand prune still works
	if h.exists("old") {
		t.Fatal("explicit prune did nothing")
	}
}

func TestAutoPruneObeysConfigRetention(t *testing.T) {
	h := newHarness(t)
	os.WriteFile(filepath.Join(h.config, "config.toml"), []byte("retention = \"2h\"\n"), 0o600)
	h.doneSession("old", 3*time.Hour)
	h.doneSession("young", time.Hour)
	os.Remove(h.stamp())
	h.ok("ps")
	if h.exists("old") || !h.exists("young") {
		t.Fatalf("old=%v young=%v", h.exists("old"), h.exists("young"))
	}
}

func TestConcurrentPrune(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		h.doneSession(n, 200*time.Hour)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e, code := h.cmd("prune"); code != 0 {
				t.Errorf("concurrent prune: exit %d: %s", code, e)
			}
		}()
	}
	wg.Wait()
	for _, n := range []string{"a", "b", "c", "d"} {
		if h.exists(n) {
			t.Errorf("%s survived", n)
		}
	}
}
