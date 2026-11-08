package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ytjohn/fastener/internal/launcher"
	"github.com/ytjohn/fastener/internal/session"
)

type failingLauncher struct{}

func (failingLauncher) Name() string { return "failing" }
func (failingLauncher) Start(session.Dir, int) (session.LauncherRecord, error) {
	return session.LauncherRecord{}, errors.New("simulated start failure")
}

// internalEnv points the CLI at throwaway state and config dirs and a profile
// whose command exists, so launch gets as far as the launcher.
func internalEnv(t *testing.T) (state string) {
	t.Helper()
	state, cfg := t.TempDir(), t.TempDir()
	t.Setenv("FASTENER_STATE_DIR", state)
	t.Setenv("FASTENER_CONFIG_DIR", cfg)
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1)")
	}
	dir := filepath.Join(cfg, "backends")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prof := fmt.Sprintf("schema_version = 1\nname = \"fake\"\ncommand = %q\n\n[headless]\nargs = []\nprompt = \"stdin\"\nparser = \"claude-stream-json\"\n\n[permissions]\nauto = []\nskip = []\n\n[check]\nversion_args = [\"--version\"]\n", truePath)
	if err := os.WriteFile(filepath.Join(dir, "fake.toml"), []byte(prof), 0o644); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestLauncherStartFailureYieldsFinalizedView(t *testing.T) {
	state := internalEnv(t)
	old := selectFunc
	selectFunc = func(string, io.Writer) (launcher.Launcher, error) { return failingLauncher{}, nil }
	t.Cleanup(func() { selectFunc = old })

	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "fake", "--name", "lf", "--cwd", t.TempDir(), "--prompt", "hi", "--json"}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit %d, want %d; stderr: %s", code, exitFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "simulated start failure") {
		t.Fatalf("stderr does not name the cause: %s", stderr.String())
	}
	var out inspectOut
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("run --json printed no view: %v\nstdout: %q", err, stdout.String())
	}
	if out.Name != "lf" || out.State != session.StateExited || out.Status.EndedBy != session.EndedByLaunchFailed ||
		!strings.Contains(out.Status.Error, "simulated start failure") {
		t.Fatalf("view not finalized as launch-failed: %+v", out)
	}
	// The finalized session stays on disk for inspection.
	if _, err := os.Stat(filepath.Join(state, "sessions", "lf")); err != nil {
		t.Fatal(err)
	}
}
