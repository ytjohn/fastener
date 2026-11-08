// Package config loads ~/.config/fastener/config.toml (optional).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ytjohn/fastener/internal/runenv"
)

type Config struct {
	Retention          string   `toml:"retention"` // used from v0.2
	DefaultPermissions string   `toml:"default_permissions"`
	HandshakeTimeout   string   `toml:"handshake_timeout"`
	Launcher           string   `toml:"launcher"`
	EnvPassthrough     []string `toml:"env_passthrough"`
	DetachKey          string   `toml:"detach_key"` // used from v0.3
}

// Dir is $FASTENER_CONFIG_DIR, else $XDG_CONFIG_HOME/fastener, else ~/.config/fastener.
func Dir() string {
	if v := os.Getenv("FASTENER_CONFIG_DIR"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return ""
		}
		return abs
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(v) {
		return filepath.Join(v, "fastener")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "fastener")
}

// BackendsDir holds user backend profiles (<name>.toml).
// It is "" when there is no config dir, never a relative path.
func BackendsDir() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "backends")
}

func defaults() Config {
	return Config{
		Retention:        "168h",
		HandshakeTimeout: "5s",
		Launcher:         "auto",
		EnvPassthrough:   append([]string(nil), runenv.DefaultPassthrough...),
		DetachKey:        "ctrl-]",
	}
}

// Load returns defaults overlaid with config.toml, if present. Unknown keys are errors.
func Load() (Config, error) {
	c := defaults()
	if Dir() == "" {
		return c, nil
	}
	path := filepath.Join(Dir(), "config.toml")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return c, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	for _, d := range []struct{ name, v string }{{"handshake_timeout", c.HandshakeTimeout}, {"retention", c.Retention}} {
		dur, err := time.ParseDuration(d.v)
		if err != nil {
			return c, fmt.Errorf("%s: %s: %w", path, d.name, err)
		}
		if dur <= 0 {
			return c, fmt.Errorf("%s: %s must be positive, got %q", path, d.name, d.v)
		}
	}
	switch c.Launcher {
	case "auto", "setsid", "systemd":
	default:
		return c, fmt.Errorf("%s: launcher must be auto, setsid or systemd, got %q", path, c.Launcher)
	}
	return c, nil
}

// Handshake returns the handshake timeout (validated by Load).
func (c Config) Handshake() time.Duration {
	d, err := time.ParseDuration(c.HandshakeTimeout)
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}
