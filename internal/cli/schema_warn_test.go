package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bumpSchema rewrites status.json with a schema_version this build does not know.
func bumpSchema(t *testing.T, h *harness, name string) {
	t.Helper()
	p := filepath.Join(h.state, "sessions", name, "status.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["schema_version"] = 99
	b, _ = json.Marshal(m)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewerSchemaWarnings(t *testing.T) {
	h := newHarness(t)
	h.start("fresh", nil)
	h.waitState("fresh", "exited", 10*time.Second)
	h.start("future", nil)
	h.waitState("future", "exited", 10*time.Second)

	// No warnings for a current-schema session, in any form.
	out, errOut, _ := h.cmd("inspect", "fresh")
	if strings.Contains(errOut, "warning") || strings.Contains(out, "warning") {
		t.Fatalf("unexpected warning: %s %s", out, errOut)
	}
	if strings.Contains(h.ok("inspect", "fresh", "--json"), `"warnings"`) {
		t.Fatal("warnings key present without warnings")
	}

	bumpSchema(t, h, "future")
	const want = "status.json has schema_version 99; this fastener understands up to 1, so some fields may be ignored"

	_, errOut, code := h.cmd("inspect", "future")
	if code != 0 || !strings.Contains(errOut, "warning: future: "+want) {
		t.Fatalf("inspect stderr (exit %d): %q", code, errOut)
	}

	out, errOut, _ = h.cmd("inspect", "future", "--json")
	var j struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil || len(j.Warnings) != 1 || j.Warnings[0] != want {
		t.Fatalf("json warnings: %v %v", j.Warnings, err)
	}
	if errOut != "" {
		t.Fatalf("--json stderr should be empty: %q", errOut)
	}

	psOut, errOut, _ := h.cmd("ps", "--all")
	if strings.Count(errOut, "warning:") != 1 || !strings.Contains(errOut, "warning: future: "+want) {
		t.Fatalf("ps stderr: %q", errOut)
	}
	if strings.Contains(psOut, "warning") {
		t.Fatalf("warning leaked to ps stdout: %q", psOut)
	}
}
