package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultsWhenMissing(t *testing.T) {
	t.Setenv("FASTENER_CONFIG_DIR", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Handshake() != 5*time.Second || c.Launcher != "auto" || len(c.EnvPassthrough) == 0 {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLoadAndUnknownKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	writeCfg(t, dir, "handshake_timeout = \"2s\"\nenv_passthrough = [\"HOME\"]\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Handshake() != 2*time.Second || len(c.EnvPassthrough) != 1 {
		t.Fatalf("got %+v", c)
	}
	writeCfg(t, dir, "bogus = 1\n")
	if _, err := Load(); err == nil {
		t.Fatal("unknown key accepted")
	}
	writeCfg(t, dir, "handshake_timeout = \"soon\"\n")
	if _, err := Load(); err == nil {
		t.Fatal("bad duration accepted")
	}
}

func TestBackendsDir(t *testing.T) {
	t.Setenv("FASTENER_CONFIG_DIR", "/x/cfg")
	if BackendsDir() != "/x/cfg/backends" {
		t.Fatal(BackendsDir())
	}
}

func writeCfg(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	writeCfg(t, dir, "launcher = \"setsid\"\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Launcher != "setsid" || c.Handshake() != 5*time.Second || c.Retention != "168h" || len(c.EnvPassthrough) == 0 || c.DetachKey != "ctrl-]" || c.RemoteWaitDuration() != 15*time.Second {
		t.Fatalf("defaults lost: %+v", c)
	}
}

func TestEmptyPassthrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	writeCfg(t, dir, "env_passthrough = []\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EnvPassthrough) != 0 {
		t.Fatalf("EnvPassthrough = %v, want empty", c.EnvPassthrough)
	}
}

func TestRejectsBadValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	for _, body := range []string{
		"handshake_timeout = \"0s\"\n",
		"handshake_timeout = \"-1s\"\n",
		"retention = \"0s\"\n",
		"retention = \"-1h\"\n",
		"launcher = \"sytemd\"\n",
		"auto_prune = \"no\"\n",
		"remote_wait = \"0s\"\n",
		"remote_wait = \"soon\"\n",
	} {
		writeCfg(t, dir, body)
		if _, err := Load(); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestRelativeXDGIgnored(t *testing.T) {
	t.Setenv("FASTENER_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if d := Dir(); !(filepath.IsAbs(d) || d == "") {
		t.Fatalf("relative XDG_CONFIG_HOME used: %q", d)
	}
}

func TestBackendsDirEmptyWithoutConfigDir(t *testing.T) {
	t.Setenv("FASTENER_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if d := BackendsDir(); d != "" {
		t.Fatalf("BackendsDir = %q, want empty", d)
	}
}

func TestRelativeConfigDirIsAbsolutized(t *testing.T) {
	t.Setenv("FASTENER_CONFIG_DIR", "rel/cfg")
	if d := Dir(); !filepath.IsAbs(d) || !strings.HasSuffix(d, "/rel/cfg") {
		t.Fatalf("Dir() = %q", d)
	}
}

func TestAutoPruneAndRetention(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	c, err := Load()
	if err != nil || !c.AutoPrune || c.RetentionDuration() != 168*time.Hour {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	writeCfg(t, dir, "auto_prune = false\nretention = \"48h\"\n")
	c, err = Load()
	if err != nil || c.AutoPrune || c.RetentionDuration() != 48*time.Hour {
		t.Fatalf("got %+v %v", c, err)
	}
}

func TestShortRetentionAcceptedAndClamped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	writeCfg(t, dir, "retention = \"30m\"\n")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load rejected a positive retention: %v", err)
	}
	if d, clamped := c.EffectiveRetention(); d != time.Hour || !clamped {
		t.Fatalf("got %s clamped=%v", d, clamped)
	}
	if c.ClampWarning() != "retention 30m is below the 1h minimum; using 1h" {
		t.Fatal(c.ClampWarning())
	}
}

func TestRemoteWait(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	writeCfg(t, dir, "remote_wait = \"40s\"\n")
	c, err := Load()
	if err != nil || c.RemoteWaitDuration() != 40*time.Second {
		t.Fatalf("remote_wait = %v, %v", c.RemoteWaitDuration(), err)
	}
}

func TestTrustDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", dir)
	writeCfg(t, dir, "trust_dirs = [\"/srv/a\", \"/srv/b/\"]\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustDirs) != 2 || !c.InTrustDirs("/srv/a") || !c.InTrustDirs("/srv/b") || c.InTrustDirs("/srv") {
		t.Fatalf("got %+v", c.TrustDirs)
	}
	if !c.Allowlisted("/x", []Authorization{{Folder: "/x"}}) || c.Allowlisted("/y", []Authorization{{Folder: "/x"}}) {
		t.Fatal("Allowlisted")
	}
	writeCfg(t, dir, "trust_dirs = [\"rel/dir\"]\n")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative trust_dirs: %v", err)
	}
}

func TestAuthorizationsConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FASTENER_CONFIG_DIR", filepath.Join(dir, "cfg"))
	if recs, err := LoadAuthorizations(); err != nil || recs != nil {
		t.Fatalf("missing file: %v %v", recs, err)
	}
	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			errs <- UpdateAuthorizations(func(r []Authorization) ([]Authorization, error) {
				return append(r, Authorization{Folder: filepath.Join("/f", strconv.Itoa(i)), AuthorizedAt: time.Now()}), nil
			})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	recs, err := LoadAuthorizations()
	if err != nil || len(recs) != n {
		t.Fatalf("%d records, %v", len(recs), err)
	}
	fi, _ := os.Stat(AuthorizationsPath())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if err := os.WriteFile(AuthorizationsPath(), []byte(`{"schema_version": 99, "authorizations": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthorizations(); err == nil {
		t.Fatal("newer schema accepted")
	}
}
