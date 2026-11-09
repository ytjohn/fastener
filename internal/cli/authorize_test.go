package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ytjohn/fastener/internal/profile"
)

const claudeFixture = `{
  "numStartups": 3,
  "lastSeen": 1794150081977,
  "ratio": 0.1,
  "projects": {
    "/elsewhere": {
      "allowedTools": [],
      "hasTrustDialogAccepted": false
    }
  }
}`

// authzHarness is a harness whose Claude config ($CLAUDE_CONFIG_DIR) and
// home are temp dirs, with a fake `claude` first on PATH that only prints a
// version. It asserts the isolation, so no test can reach the real
// ~/.claude.json. It returns the Claude config path (created from
// claudeFixture unless noConfig).
func authzHarness(t *testing.T, noConfig bool) (*harness, string) {
	t.Helper()
	realHome, _ := os.UserHomeDir()
	h := newHarness(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claudeDir, home, bin := filepath.Join(base, "claude"), filepath.Join(base, "home"), filepath.Join(base, "bin")
	for _, d := range []string{claudeDir, home, bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho '9.9.9 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home) // the harness passes HOME and PATH through
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	h.extraEnv = append(h.extraEnv, "CLAUDE_CONFIG_DIR="+claudeDir)
	cfg := filepath.Join(claudeDir, ".claude.json")
	if !strings.HasPrefix(cfg, base+"/") || (realHome != "" && cfg == filepath.Join(realHome, ".claude.json")) {
		t.Fatalf("Claude config %s is not isolated", cfg)
	}
	if !strings.HasPrefix(h.config, os.TempDir()) {
		t.Fatalf("FASTENER_CONFIG_DIR %s is not a temp dir", h.config)
	}
	if !noConfig {
		if err := os.WriteFile(cfg, []byte(claudeFixture), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h, cfg
}

func newFolder(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type listJSON struct {
	SchemaVersion  int `json:"schema_version"`
	Authorizations []struct {
		Folder           string  `json:"folder"`
		Key              string  `json:"key"`
		AuthorizedAt     string  `json:"authorized_at"`
		VerifiedAt       *string `json:"verified_at"`
		ClaudeVersion    string  `json:"claude_version"`
		ClaudeConfigPath string  `json:"claude_config"`
	} `json:"authorizations"`
	TrustDirs []string `json:"trust_dirs"`
}

func trusted(t *testing.T, cfg, key string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(mustReadFile(t, cfg)), &m); err != nil {
		t.Fatal(err)
	}
	p, _ := m["projects"].(map[string]any)
	e, _ := p[key].(map[string]any)
	v, _ := e["hasTrustDialogAccepted"].(bool)
	return v
}

func mustReadFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuthorizeListRevoke(t *testing.T) {
	h, cfg := authzHarness(t, false)
	dir := newFolder(t)

	out := h.ok("authorize", "claude-folder-trust", dir)
	for _, want := range []string{"trusted " + dir, "folder: " + dir, "key:    " + dir + " (the folder itself)", "config: " + cfg, "backup: " + filepath.Join(h.config, "backups")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !trusted(t, cfg, dir) {
		t.Fatal("not trusted")
	}

	var l listJSON
	if err := json.Unmarshal([]byte(h.ok("authorize", "--list", "--json")), &l); err != nil {
		t.Fatal(err)
	}
	if l.SchemaVersion != 1 || len(l.Authorizations) != 1 || l.TrustDirs == nil {
		t.Fatalf("list %+v", l)
	}
	a := l.Authorizations[0]
	if a.Folder != dir || a.Key != dir || a.ClaudeConfigPath != cfg || a.VerifiedAt == nil || a.ClaudeVersion != "9.9.9 (Claude Code)" || a.AuthorizedAt == "" {
		t.Fatalf("record %+v", a)
	}
	if out := h.ok("authorize", "--list"); !strings.Contains(out, dir) || !strings.Contains(out, "9.9.9") {
		t.Fatalf("list:\n%s", out)
	}

	// Again: already trusted, nothing written.
	before := mustReadFile(t, cfg)
	fi1, _ := os.Stat(cfg)
	var aj struct {
		AlreadyTrusted bool   `json:"already_trusted"`
		Key            string `json:"key"`
	}
	if err := json.Unmarshal([]byte(h.ok("authorize", "claude-folder-trust", "--json", dir)), &aj); err != nil || !aj.AlreadyTrusted || aj.Key != dir {
		t.Fatalf("second authorize: %+v %v", aj, err)
	}
	if out := h.ok("authorize", "claude-folder-trust", dir); !strings.Contains(out, "already trusted") {
		t.Fatalf("second authorize: %s", out)
	}
	fi2, _ := os.Stat(cfg)
	if !fi1.ModTime().Equal(fi2.ModTime()) || mustReadFile(t, cfg) != before {
		t.Fatal("already-trusted run wrote the file")
	}

	// Revoke leaves Claude's config alone and says so.
	out = h.ok("authorize", "--revoke", dir)
	if !strings.Contains(out, "revoked "+dir) || !strings.Contains(out, "Claude's config was not changed") {
		t.Fatalf("revoke: %s", out)
	}
	if mustReadFile(t, cfg) != before {
		t.Fatal("revoke changed Claude's config")
	}
	if err := json.Unmarshal([]byte(h.ok("authorize", "--list", "--json")), &l); err != nil || len(l.Authorizations) != 0 {
		t.Fatalf("after revoke: %+v %v", l, err)
	}
	if out := h.ok("authorize", "--list"); !strings.Contains(out, "no folders") {
		t.Fatalf("empty list: %s", out)
	}
	if _, errOut, code := h.cmd("authorize", "--revoke", dir); code != 1 || !strings.Contains(errOut, "not authorized") {
		t.Fatalf("second revoke: %d %s", code, errOut)
	}
}

func TestAuthorizeRevokeJSONAndTrustDirs(t *testing.T) {
	h, cfg := authzHarness(t, false)
	dir := newFolder(t)
	h.writeConfig("trust_dirs = [" + `"` + dir + `"` + "]\n")
	h.ok("authorize", "claude-folder-trust", dir)
	var l listJSON
	if err := json.Unmarshal([]byte(h.ok("authorize", "--list", "--json")), &l); err != nil || len(l.TrustDirs) != 1 || l.TrustDirs[0] != dir {
		t.Fatalf("list %+v %v", l, err)
	}
	before := mustReadFile(t, cfg)
	out, errOut, code := h.cmd("authorize", "--revoke", dir, "--json")
	var r struct {
		Revoked     bool `json:"revoked"`
		InTrustDirs bool `json:"in_trust_dirs"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || !r.Revoked || !r.InTrustDirs || !strings.Contains(errOut, "still listed in trust_dirs") {
		t.Fatalf("revoke: %d %s %s", code, out, errOut)
	}
	if mustReadFile(t, cfg) != before {
		t.Fatal("revoke changed Claude's config")
	}
}

func TestAuthorizeKeyIsRepoRoot(t *testing.T) {
	if _, err := lookGit(); err != nil {
		t.Skip("no git")
	}
	h, cfg := authzHarness(t, false)
	repo := newFolder(t)
	sub := filepath.Join(repo, "pkg")
	_ = os.Mkdir(sub, 0o755)
	gitInit(t, repo)
	out := h.ok("authorize", "claude-folder-trust", sub)
	if !strings.Contains(out, "key:    "+repo+" (the git repository root") {
		t.Fatalf("output:\n%s", out)
	}
	if !trusted(t, cfg, repo) || trusted(t, cfg, sub) {
		t.Fatal("wrong key trusted")
	}
}

func TestAuthorizeFailures(t *testing.T) {
	h, cfg := authzHarness(t, false)
	before := mustReadFile(t, cfg)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"claude-folder-trust", filepath.Join(newFolder(t), "missing")}, "no such file"},
		{[]string{"claude-folder-trust", os.Getenv("HOME")}, "home directory"},
		{[]string{"claude-folder-trust", "/"}, "filesystem root"},
	} {
		_, errOut, code := h.cmd(append([]string{"authorize"}, c.args...)...)
		if code != 1 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, %s", c.args, code, errOut)
		}
	}
	if mustReadFile(t, cfg) != before {
		t.Fatal("a refused authorize changed Claude's config")
	}
	if _, err := os.Stat(filepath.Join(h.config, "authorizations.json")); !os.IsNotExist(err) {
		t.Fatal("a refused folder was recorded")
	}

	h2, _ := authzHarness(t, true)
	_, errOut, code := h2.cmd("authorize", "claude-folder-trust", newFolder(t))
	if code != 1 || !strings.Contains(errOut, "run claude once first") {
		t.Fatalf("no Claude config: %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(h2.config, "authorizations.json")); !os.IsNotExist(err) {
		t.Fatal("recorded without a Claude config")
	}
}

func TestAuthorizeUsage(t *testing.T) {
	h, cfg := authzHarness(t, false)
	before := mustReadFile(t, cfg)
	dir := newFolder(t)
	for _, args := range [][]string{
		{},
		{"claude-folder-trust"},
		{"claude-folder-trust", dir, dir},
		{"codex-folder-trust", dir},
		{"--list", dir},
		{"--list", "--revoke", dir},
		{"--revoke", dir, dir},
		{"--revoke", ""},
		{"--bogus"},
	} {
		if _, errOut, code := h.cmd(append([]string{"authorize"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errOut)
		}
	}
	out, _, code := h.cmd("authorize", "-h")
	if code != 0 {
		t.Fatalf("-h exit %d", code)
	}
	_, errOut, _ := h.cmd("authorize", "-h")
	if !strings.Contains(out+errOut, "edits Claude Code's own config file") || !strings.Contains(out+errOut, "trust dialog") {
		t.Fatalf("help:\n%s%s", out, errOut)
	}
	if mustReadFile(t, cfg) != before {
		t.Fatal("usage errors changed Claude's config")
	}
}

// TestUntrustedHintRuns checks that the command the claude profile's
// untrusted_hint tells users to run is exactly this command.
func TestUntrustedHintRuns(t *testing.T) {
	h, cfg := authzHarness(t, false)
	p, err := profile.Load("claude", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := newFolder(t)
	hint := strings.ReplaceAll(p.Remote.UntrustedHint, "{dir}", dir)
	f := strings.Fields(hint)
	if len(f) != 4 || f[0] != "fastener" || f[1] != "authorize" || f[2] != "claude-folder-trust" || f[3] != dir {
		t.Fatalf("hint %q", hint)
	}
	h.ok(f[1:]...)
	if !trusted(t, cfg, dir) {
		t.Fatal("hint command did not trust the folder")
	}
}

func lookGit() (string, error) { return exec.LookPath("git") }

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

func TestAuthorizeRevokeNamesSharedKey(t *testing.T) {
	if _, err := lookGit(); err != nil {
		t.Skip("no git")
	}
	h, cfg := authzHarness(t, false)
	repo := newFolder(t)
	a, b := filepath.Join(repo, "a"), filepath.Join(repo, "b")
	_ = os.Mkdir(a, 0o755)
	_ = os.Mkdir(b, 0o755)
	gitInit(t, repo)
	h.ok("authorize", "claude-folder-trust", a)
	h.ok("authorize", "claude-folder-trust", b) // same key: already trusted
	before := mustReadFile(t, cfg)
	out := h.ok("authorize", "--revoke", a)
	want := "still authorized with the same Claude key " + repo
	if !strings.Contains(out, want) || !strings.Contains(out, b) {
		t.Fatalf("revoke:\n%s", out)
	}
	if strings.Index(out, want) > strings.Index(out, "Claude's config was not changed") {
		t.Fatal("shared-key note should come first")
	}
	if mustReadFile(t, cfg) != before {
		t.Fatal("revoke changed Claude's config")
	}
}

func TestAuthorizeRefusesOtherClaudeConfigs(t *testing.T) {
	h, cfg := authzHarness(t, false)
	before := mustReadFile(t, cfg)
	h.extraEnv = append(h.extraEnv, "CLAUDE_CODE_CUSTOM_OAUTH_URL=https://example.invalid")
	_, errOut, code := h.cmd("authorize", "claude-folder-trust", newFolder(t))
	if code != 1 || !strings.Contains(errOut, ".claude-custom-oauth.json") {
		t.Fatalf("custom oauth: %d %s", code, errOut)
	}
	h.extraEnv = h.extraEnv[:len(h.extraEnv)-1]
	legacy := filepath.Join(filepath.Dir(cfg), ".config.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = h.cmd("authorize", "claude-folder-trust", newFolder(t))
	if code != 1 || !strings.Contains(errOut, legacy) {
		t.Fatalf("legacy: %d %s", code, errOut)
	}
	if mustReadFile(t, cfg) != before || mustReadFile(t, legacy) != "{}" {
		t.Fatal("a refused authorize wrote a config")
	}
	if _, err := os.Stat(filepath.Join(h.config, "authorizations.json")); !os.IsNotExist(err) {
		t.Fatal("recorded despite the refusal")
	}
}
