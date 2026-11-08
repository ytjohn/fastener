package config

import (
	"os"
	"path/filepath"
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
	if c.Launcher != "setsid" || c.Handshake() != 5*time.Second || c.Retention != "168h" || len(c.EnvPassthrough) == 0 || c.DetachKey != "ctrl-]" {
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
		"retention = \"30m\"\n",
		"launcher = \"sytemd\"\n",
		"auto_prune = \"no\"\n",
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
