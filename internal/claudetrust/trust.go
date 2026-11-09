package claudetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

// TrustField is the per-project field Claude Code checks.
const TrustField = "hasTrustDialogAccepted"

// KeepBackups is how many backups of Claude's config are kept.
const KeepBackups = 5

// ErrNoClaudeConfig means Claude's config file does not exist yet.
var ErrNoClaudeConfig = errors.New("Claude Code's config file does not exist; run claude once first")

// ConfigPath is Claude Code's global config: $CLAUDE_CONFIG_DIR/.claude.json
// when CLAUDE_CONFIG_DIR is set, else ~/.claude.json.
func ConfigPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		abs, err := filepath.Abs(d)
		if err != nil {
			return "", err
		}
		return filepath.Join(abs, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// Options for Trust. Zero durations take the defaults.
type Options struct {
	ConfigPath  string // Claude's config file (required)
	BackupDir   string // where backups go (required): <fastener config>/backups
	LockTimeout time.Duration
	LockStale   time.Duration
}

// Result of Trust.
type Result struct {
	Key            string `json:"key"`
	ConfigPath     string `json:"claude_config"`
	AlreadyTrusted bool   `json:"already_trusted"`
	Backup         string `json:"backup,omitempty"` // the backup written, if the file was changed
}

// Trust sets projects[key].hasTrustDialogAccepted = true in Claude's config
// under Claude's own lock, changing nothing else, then re-reads the file to
// verify. A key already trusted is left alone (nothing is written).
func Trust(key string, o Options) (Result, error) {
	res := Result{Key: key, ConfigPath: o.ConfigPath}
	if o.ConfigPath == "" || o.BackupDir == "" {
		return res, errors.New("claudetrust: ConfigPath and BackupDir are required")
	}
	if o.LockTimeout <= 0 {
		o.LockTimeout = DefaultLockTimeout
	}
	if o.LockStale <= 0 {
		o.LockStale = DefaultLockStale
	}
	// proper-lockfile locks the file's real path, and the write must replace
	// the real file rather than a symlink to it.
	real, err := filepath.EvalSymlinks(o.ConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%s: %w", o.ConfigPath, ErrNoClaudeConfig)
	}
	if err != nil {
		return res, err
	}
	lock, err := acquireLock(real+".lock", o.LockTimeout, o.LockStale)
	if err != nil {
		return res, err
	}
	changed, backup, err := trustLocked(real, key, o.BackupDir)
	if rerr := lock.release(); rerr != nil && err == nil {
		err = fmt.Errorf("releasing lock %s.lock: %w", real, rerr)
	}
	res.AlreadyTrusted, res.Backup = !changed, backup
	if err != nil {
		return res, err
	}
	if err := Verify(o.ConfigPath, key); err != nil {
		return res, err
	}
	return res, nil
}

// trustLocked does the read-modify-write; the caller holds the lock.
func trustLocked(path, key, backupDir string) (changed bool, backup string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, "", err
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return false, "", err
	}
	doc, err := decodeOrdered(orig)
	if err != nil {
		return false, "", fmt.Errorf("%s: parsing: %w", path, err)
	}
	root, ok := doc.(*object)
	if !ok {
		return false, "", fmt.Errorf("%s: the top level is not a JSON object", path)
	}
	projects := newObject()
	if v, ok := root.get("projects"); ok {
		if projects, ok = v.(*object); !ok {
			return false, "", fmt.Errorf("%s: projects is not a JSON object", path)
		}
	}
	entry := newObject()
	if v, ok := projects.get(key); ok {
		if entry, ok = v.(*object); !ok {
			return false, "", fmt.Errorf("%s: projects[%q] is not a JSON object", path, key)
		}
	}
	if v, ok := entry.get(TrustField); ok {
		if raw, ok := v.(json.RawMessage); ok && string(raw) == "true" {
			return false, "", nil
		}
	}
	entry.set(TrustField, json.RawMessage("true"))
	projects.set(key, entry)
	root.set("projects", projects)
	out := encodeStringify(root)
	if bytes.HasSuffix(orig, []byte("\n")) {
		out = append(out, '\n')
	}
	if err := checkOnlyTrustChanged(orig, out, key); err != nil {
		return false, "", fmt.Errorf("%s: refusing to write: %w", path, err)
	}
	if backup, err = writeBackup(backupDir, orig); err != nil {
		return false, "", fmt.Errorf("backing up %s: %w", path, err)
	}
	if err := session.WriteFileAtomic(path, out, fi.Mode().Perm()); err != nil {
		return false, backup, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, backup, nil
}

// decodeLoose parses JSON into map[string]any with exact numbers.
func decodeLoose(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// checkOnlyTrustChanged is an independent check, with encoding/json, that
// out equals orig with only projects[key].hasTrustDialogAccepted set to true.
func checkOnlyTrustChanged(orig, out []byte, key string) error {
	want, err := decodeLoose(orig)
	if err != nil {
		return err
	}
	got, err := decodeLoose(out)
	if err != nil {
		return fmt.Errorf("the new content does not parse: %w", err)
	}
	root := want.(map[string]any)
	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	entry, _ := projects[key].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[key] = entry
	}
	entry[TrustField] = true
	if !reflect.DeepEqual(want, got) {
		return errors.New("the re-encoded file would change more than the trust field")
	}
	return nil
}

// Verify re-reads Claude's config and checks the trust field is true.
func Verify(path, key string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("verifying: %w", err)
	}
	v, err := decodeLoose(b)
	if err != nil {
		return fmt.Errorf("verifying %s: %w", path, err)
	}
	root, _ := v.(map[string]any)
	projects, _ := root["projects"].(map[string]any)
	entry, _ := projects[key].(map[string]any)
	if t, _ := entry[TrustField].(bool); !t {
		return fmt.Errorf("verifying %s: projects[%q].%s is not true after writing it", path, key, TrustField)
	}
	return nil
}

// IsTrusted reports whether Claude's config has the key trusted.
func IsTrusted(path, key string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	return Verify(path, key) == nil, nil
}

const backupPrefix = "claude.json."

// writeBackup saves b as <dir>/claude.json.<UTC timestamp> (0600) and keeps
// the newest KeepBackups.
func writeBackup(dir string, b []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(dir, backupPrefix+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := session.WriteFileAtomic(name, b, 0o600); err != nil {
		return "", err
	}
	pruneBackups(dir)
	return name, nil
}

func pruneBackups(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if n := e.Name(); e.Type().IsRegular() && strings.HasPrefix(n, backupPrefix) {
			names = append(names, n)
		}
	}
	sort.Strings(names) // timestamps sort lexically
	for len(names) > KeepBackups {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}
