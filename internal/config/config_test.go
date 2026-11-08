package config

import (
	"os"
	"path/filepath"
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
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("handshake_timeout = \"2s\"\nenv_passthrough = [\"HOME\"]\n"), 0o600)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Handshake() != 2*time.Second || len(c.EnvPassthrough) != 1 {
		t.Fatalf("got %+v", c)
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("bogus = 1\n"), 0o600)
	if _, err := Load(); err == nil {
		t.Fatal("unknown key accepted")
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("handshake_timeout = \"soon\"\n"), 0o600)
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
