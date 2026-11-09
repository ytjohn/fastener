package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

// AuthorizationsSchemaVersion is the schema_version of authorizations.json.
const AuthorizationsSchemaVersion = 1

// Authorization records one `fastener authorize claude-folder-trust`.
// Every recorded folder is on the trust allowlist; a record without
// VerifiedAt was added but Claude's config was not (yet) confirmed.
type Authorization struct {
	Folder        string     `json:"folder"`                   // resolved real path
	Key           string     `json:"key,omitempty"`            // Claude's trust key (the folder or its git root)
	ClaudeConfig  string     `json:"claude_config,omitempty"`  // the .claude.json that was written
	AuthorizedAt  time.Time  `json:"authorized_at"`            // when the folder was added
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`    // when the trust field was confirmed
	ClaudeVersion string     `json:"claude_version,omitempty"` // `claude --version`, if available
}

type authorizationsFile struct {
	SchemaVersion  int             `json:"schema_version"`
	Authorizations []Authorization `json:"authorizations"`
}

// AuthorizationsPath is <config dir>/authorizations.json, the machine-written
// allowlist and record of folder-trust authorizations ("" without a config
// dir). It is kept apart from the hand-edited config.toml.
func AuthorizationsPath() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "authorizations.json")
}

// ErrNoConfigDir means no config directory could be determined.
var ErrNoConfigDir = errors.New("no config directory (set FASTENER_CONFIG_DIR or HOME)")

// LoadAuthorizations reads authorizations.json; a missing file is no records.
func LoadAuthorizations() ([]Authorization, error) {
	p := AuthorizationsPath()
	if p == "" {
		return nil, ErrNoConfigDir
	}
	return readAuthorizations(p)
}

func readAuthorizations(p string) ([]Authorization, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f authorizationsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if f.SchemaVersion > AuthorizationsSchemaVersion {
		return nil, fmt.Errorf("%s: schema_version %d is newer than this fastener understands (%d)", p, f.SchemaVersion, AuthorizationsSchemaVersion)
	}
	return f.Authorizations, nil
}

// UpdateAuthorizations applies fn to the records under an exclusive flock on
// authorizations.json.lock and writes the result atomically, so concurrent
// fastener commands do not lose each other's records.
func UpdateAuthorizations(fn func([]Authorization) ([]Authorization, error)) error {
	p := AuthorizationsPath()
	if p == "" {
		return ErrNoConfigDir
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking %s: %w", p, err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	recs, err := readAuthorizations(p)
	if err != nil {
		return err
	}
	recs, err = fn(recs)
	if err != nil {
		return err
	}
	if recs == nil {
		recs = []Authorization{}
	}
	b, err := json.MarshalIndent(authorizationsFile{SchemaVersion: AuthorizationsSchemaVersion, Authorizations: recs}, "", "  ")
	if err != nil {
		return err
	}
	return session.WriteFileAtomic(p, append(b, '\n'), 0o600)
}

// Allowlisted reports whether folder is on the trust allowlist: recorded in
// recs, or listed in config.toml's trust_dirs. folder must already be
// resolved (absolute, symlinks resolved); trust_dirs entries are compared
// both as written and with symlinks resolved.
func (c Config) Allowlisted(folder string, recs []Authorization) bool {
	for _, r := range recs {
		if r.Folder == folder {
			return true
		}
	}
	return c.InTrustDirs(folder)
}

// InTrustDirs reports whether config.toml's trust_dirs lists folder.
func (c Config) InTrustDirs(folder string) bool {
	for _, d := range c.TrustDirs {
		d = filepath.Clean(d)
		if d == folder {
			return true
		}
		if r, err := filepath.EvalSymlinks(d); err == nil && r == folder {
			return true
		}
	}
	return false
}
