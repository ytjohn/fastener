package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmokeBuiltins runs `fastener check` against each built-in backend whose
// CLI is installed. Opt in with FASTENER_SMOKE=1; it spends real model calls.
// FASTENER_SMOKE_ARGS adds flags, e.g. "--model haiku".
func TestSmokeBuiltins(t *testing.T) {
	if os.Getenv("FASTENER_SMOKE") != "1" {
		t.Skip("set FASTENER_SMOKE=1 to run live backend checks")
	}
	extra := strings.Fields(os.Getenv("FASTENER_SMOKE_ARGS"))
	for _, b := range []struct {
		name, bin string
		args      []string
	}{
		{"claude", "claude", nil},
		{"codex", "codex", nil},
		{"pi", "pi", []string{"--permissions", "skip"}},
	} {
		t.Run(b.name, func(t *testing.T) {
			if _, err := exec.LookPath(b.bin); err != nil {
				t.Skipf("%s not installed", b.bin)
			}
			h := newHarness(t)
			h.extraEnv = os.Environ() // real credentials and config
			r, code := h.check(append(append([]string{b.name}, b.args...), extra...)...)
			for _, s := range r[0].Steps {
				t.Logf("step %-12s %-6s %s %s", s.Name, s.Status, s.Detail, s.Hint)
			}
			t.Logf("session: %s", r[0].Session)
			// FASTENER_SMOKE_SAVE=<dir> copies the raw output.log (needs --keep).
			if dir := os.Getenv("FASTENER_SMOKE_SAVE"); dir != "" && r[0].Session != "" {
				var v struct {
					Paths struct {
						OutputLog string `json:"output_log"`
					} `json:"paths"`
				}
				if err := json.Unmarshal([]byte(h.ok("inspect", r[0].Session, "--json")), &v); err == nil {
					if data, err := os.ReadFile(v.Paths.OutputLog); err == nil {
						os.MkdirAll(dir, 0o755)
						os.WriteFile(filepath.Join(dir, b.name+".log"), data, 0o600)
					}
				}
			}
			if code != 0 || !r[0].Passed {
				t.Fatalf("check %s failed: %+v", b.name, r[0].Steps)
			}
		})
	}
}
