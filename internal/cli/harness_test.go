package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/proc"
)

var fastenerBin, fakeBin string

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "fastener-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	build := func(out, pkg string) bool {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n", pkg, err)
			return false
		}
		return true
	}
	fastenerBin = filepath.Join(dir, "fastener")
	fakeBin = filepath.Join(dir, "fakeagent")
	if !build(fastenerBin, "github.com/ytjohn/fastener/cmd/fastener") ||
		!build(fakeBin, "github.com/ytjohn/fastener/internal/testbin/fakeagent") {
		return 1
	}
	return m.Run()
}

const fakeProfile = `schema_version = 1
name    = "fake"
command = %q

[headless]
args   = []
prompt = "stdin"
parser = "claude-stream-json"

[options]
model = ["--model", "{value}"]

[permissions]
auto = []
skip = []

[check]
version_args = ["--version"]
diagnose = [
  { match = 'FAKE-NOT-LOGGED-IN', hint = "auth: log in to fake" },
]
`

// fakeBridgeProfile is fakeagent with a [remote] section, standing in for
// `claude remote-control` (run it with --env FAKE_BRIDGE=1).
const fakeBridgeProfile = `schema_version = 1
name    = "fakebridge"
command = %q

[headless]
args   = []
prompt = "stdin"
parser = "claude-stream-json"

[permissions]
auto = ["--auto"]
skip = ["--skip"]

[remote]
args              = ["bridge", "--name", "{name}"]
untrusted_pattern = '(?i)workspace not trusted'
dedupe_frames     = true

[remote.permissions]
skip = ["--permission-mode", "bypassPermissions"]
`

type harness struct {
	t                   *testing.T
	state, config, work string
	extraEnv            []string
	// launcher is written into every config.toml. It is setsid by default, so
	// the suite behaves the same whether or not the machine has a user systemd
	// (auto would pick systemd on a developer box and setsid in CI);
	// systemd_test.go covers the systemd launcher explicitly.
	launcher string
	// softNofile, when set, is the soft RLIMIT_NOFILE the CLI is started with.
	softNofile string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, state: t.TempDir(), config: t.TempDir(), work: t.TempDir(), launcher: "setsid"}
	h.writeProfile("fake", fmt.Sprintf(fakeProfile, fakeBin))
	h.writeConfig("")
	t.Cleanup(h.cleanup)
	return h
}

// writeConfig replaces config.toml with body plus the harness's launcher line.
func (h *harness) writeConfig(body string) error {
	h.t.Helper()
	if h.launcher != "" {
		body = fmt.Sprintf("launcher = %q\n", h.launcher) + body
	}
	err := os.WriteFile(filepath.Join(h.config, "config.toml"), []byte(body), 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	return err
}

func (h *harness) writeProfile(name, body string) {
	dir := filepath.Join(h.config, "backends")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) cmd(args ...string) (string, string, int) {
	h.t.Helper()
	return h.run(args...)
}

// run is cmd without t.Helper, safe to call from goroutines. A failure to
// start the process reports exit code -1.
func (h *harness) run(args ...string) (string, string, int) {
	c := exec.Command(fastenerBin, args...)
	if h.softNofile != "" {
		c = exec.Command("/bin/sh", append([]string{"-c", `ulimit -Sn "$0" && exec "$@"`, h.softNofile, fastenerBin}, args...)...)
	}
	// extraEnv first: with duplicate keys the last entry wins, so the
	// harness's own values must come after it.
	c.Env = append(append([]string{}, h.extraEnv...),
		"FASTENER_STATE_DIR="+h.state, "FASTENER_CONFIG_DIR="+h.config,
		"PATH="+os.Getenv("PATH"), "HOME="+os.Getenv("HOME"),
		"TMPDIR="+h.work)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return o.String(), e.String(), -1
	}
	return o.String(), e.String(), code
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	out, errOut, code := h.cmd(args...)
	if code != 0 {
		h.t.Fatalf("fastener %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return strings.TrimSpace(out)
}

// start launches the fake backend; env entries become --env flags.
func (h *harness) start(name string, env []string, extra ...string) {
	h.t.Helper()
	args := []string{"run", "fake", "--name", name, "--cwd", h.work, "--prompt", "hello"}
	for _, e := range env {
		args = append(args, "--env", e)
	}
	h.ok(append(args, extra...)...)
}

type inspectJSON struct {
	State   string `json:"state"`
	Orphans int    `json:"orphans"`
	Status  struct {
		EndedBy        string `json:"ended_by"`
		ExitCode       *int   `json:"exit_code"`
		Signal         string `json:"signal"`
		Error          string `json:"error"`
		PGID           int    `json:"pgid"`
		ConversationID string `json:"conversation_id"`
		FinalMessage   string `json:"final_message"`
		AgentError     *bool  `json:"agent_error"`
		Monitor        *struct {
			PID int `json:"pid"`
		} `json:"monitor"`
	} `json:"status"`
	Plan *struct {
		ProfileSHA256 string `json:"profile_sha256"`
	} `json:"plan"`
}

func (h *harness) inspect(name string) inspectJSON {
	h.t.Helper()
	var v inspectJSON
	if err := json.Unmarshal([]byte(h.ok("inspect", name, "--json")), &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) waitState(name, state string, timeout time.Duration) inspectJSON {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v := h.inspect(name)
		if v.State == state {
			return v
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: state %s after %s, want %s", name, v.State, timeout, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) cleanup() {
	out, _, code := h.cmd("ps", "--all", "--json")
	if code != 0 {
		return
	}
	var rows []struct {
		Name string `json:"name"`
	}
	json.Unmarshal([]byte(out), &rows)
	for _, r := range rows {
		if _, e, code := h.cmd("rm", "--force", r.Name); code != 0 {
			h.t.Logf("cleanup: rm --force %s: exit %d: %s", r.Name, code, e)
		}
	}
}

// pidAlive reports whether pid exists and is not a zombie (Linux /proc, macOS sysctl).
func pidAlive(pid int) bool {
	_, err := proc.StartTime(pid)
	return err == nil
}
