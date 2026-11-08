package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// psNames runs `ps --json` with args and returns the sorted session names.
func (h *harness) psNames(args ...string) []string {
	h.t.Helper()
	out := h.ok(append([]string{"ps", "--json"}, args...)...)
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		h.t.Fatalf("ps output %q: %v", out, err)
	}
	names := []string{}
	for _, r := range rows {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

func wantNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ps names = %v, want %v", got, want)
	}
}

// psFilterFixture: a-run (running, label bead=b1, team set), a-done (exited,
// bead=b2), b-run (running, backend other, bead=b1).
func psFilterFixture(t *testing.T) *harness {
	h := newHarness(t)
	h.writeProfile("other", fmt.Sprintf(strings.Replace(fakeProfile, `name    = "fake"`, `name    = "other"`, 1), fakeBin))
	h.start("a-run", []string{"FAKE_SLEEP=30s"}, "--label", "bead=b1", "--label", "team=x")
	h.start("a-done", nil, "--label", "bead=b2")
	h.ok("wait", "a-done", "--timeout", "10s")
	h.ok("run", "other", "--name", "b-run", "--cwd", h.work, "--prompt", "hi", "--env", "FAKE_SLEEP=30s", "--label", "bead=b1")
	h.waitState("a-run", "running", 5*time.Second)
	h.waitState("b-run", "running", 5*time.Second)
	return h
}

func TestPsFilterLabelValue(t *testing.T) {
	h := psFilterFixture(t)
	wantNames(t, h.psNames("--filter", "label=bead=b1"), "a-run", "b-run")
	wantNames(t, h.psNames("--filter", "label=bead=b2"), "a-done")
}

func TestPsFilterLabelExists(t *testing.T) {
	h := psFilterFixture(t)
	wantNames(t, h.psNames("--filter", "label=team"), "a-run")
	wantNames(t, h.psNames("--filter", "label=bead"), "a-done", "a-run", "b-run")
}

func TestPsFilterState(t *testing.T) {
	h := psFilterFixture(t)
	wantNames(t, h.psNames("--filter", "state=running"), "a-run", "b-run")
	wantNames(t, h.psNames("--filter", "state=exited"), "a-done")
}

func TestPsFilterBackend(t *testing.T) {
	h := psFilterFixture(t)
	wantNames(t, h.psNames("--filter", "backend=other"), "b-run")
}

func TestPsFilterNameGlob(t *testing.T) {
	h := psFilterFixture(t)
	wantNames(t, h.psNames("--filter", "name=a-*"), "a-done", "a-run")
	wantNames(t, h.psNames("--filter", "name=?-run"), "a-run", "b-run")
}

func TestPsFilterAnded(t *testing.T) {
	h := psFilterFixture(t)
	wantNames(t, h.psNames("--filter", "label=bead=b1", "--filter", "backend=other"), "b-run")
	wantNames(t, h.psNames("--filter", "label=bead=b1", "--filter", "state=exited"))
}

func TestPsFilterWithAll(t *testing.T) {
	h := psFilterFixture(t)
	// Age a-done past the 24h window by rewriting its status end time.
	matches, _ := filepath.Glob(filepath.Join(h.state, "*", "a-done", "status.json"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(h.state, "**", "status.json"))
	}
	if len(matches) != 1 {
		t.Skipf("cannot locate status.json (%v)", matches)
	}
	b, _ := os.ReadFile(matches[0])
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	m["ended_at"] = old
	b, _ = json.Marshal(m)
	os.WriteFile(matches[0], b, 0o644)
	cr := filepath.Join(filepath.Dir(matches[0]), "session.json")
	sb, _ := os.ReadFile(cr)
	var sm map[string]any
	json.Unmarshal(sb, &sm)
	sm["created_at"] = old
	sb, _ = json.Marshal(sm)
	os.WriteFile(cr, sb, 0o644)

	// Filter applies on top of the 24h window: old exited session hidden.
	wantNames(t, h.psNames("--filter", "state=exited"))
	wantNames(t, h.psNames("--all", "--filter", "state=exited"), "a-done")
	wantNames(t, h.psNames("--all", "--filter", "label=bead=b1"), "a-run", "b-run")
}

func TestPsFilterUsageErrors(t *testing.T) {
	h := psFilterFixture(t)
	for _, f := range []string{"bogus=1", "nokey", "state=zombie", "=x", "label=", "label==v", "name=[", "backend="} {
		_, errOut, code := h.cmd("ps", "--filter", f)
		if code != 2 {
			t.Errorf("--filter %q: exit %d, want 2 (%s)", f, code, errOut)
		}
	}
	_, errOut, _ := h.cmd("ps", "--filter", "bogus=1")
	for _, k := range []string{"label", "state", "backend", "name"} {
		if !strings.Contains(errOut, k) {
			t.Errorf("unknown-key error should list %q: %s", k, errOut)
		}
	}
	_, errOut, _ = h.cmd("ps", "--filter", "state=zombie")
	if !strings.Contains(errOut, "restore-failed") {
		t.Errorf("bad state error should list valid states: %s", errOut)
	}
}

func TestPsFilterNoMatchEmptyArray(t *testing.T) {
	h := psFilterFixture(t)
	out := h.ok("ps", "--json", "--filter", "label=bead=nope")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("got %q, want []", out)
	}
}
