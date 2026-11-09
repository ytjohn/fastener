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
)

// TrustField is the per-project field Claude Code checks.
const TrustField = "hasTrustDialogAccepted"

// KeepBackups is how many backups of Claude's config are kept.
const KeepBackups = 5

// ErrNoClaudeConfig means Claude's config file does not exist yet.
var ErrNoClaudeConfig = errors.New("Claude Code's config file does not exist; run claude once first")

// ErrUnsupportedConfig means Claude Code would use a config file other than
// the one Fastener knows how to edit.
var ErrUnsupportedConfig = errors.New("Claude Code would use a different config file")

// ConfigPath is Claude Code's global config: $CLAUDE_CONFIG_DIR/.claude.json
// when CLAUDE_CONFIG_DIR is set, else ~/.claude.json. It refuses
// (ErrUnsupportedConfig, naming the file Claude would use) when Claude would
// pick another file: CLAUDE_CODE_CUSTOM_OAUTH_URL makes the name
// .claude-custom-oauth.json, and a non-empty legacy .config.json in Claude's
// config home ($CLAUDE_CONFIG_DIR, else ~/.claude) usually takes precedence.
func ConfigPath() (string, error) {
	dir, home := os.Getenv("CLAUDE_CONFIG_DIR"), ""
	if dir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = h
	}
	base, claudeHome := dir, dir
	if dir == "" {
		base, claudeHome = home, filepath.Join(home, ".claude")
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	if claudeHome, err = filepath.Abs(claudeHome); err != nil {
		return "", err
	}
	if os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL") != "" {
		return "", fmt.Errorf("%w: CLAUDE_CODE_CUSTOM_OAUTH_URL is set, so Claude Code uses %s; fastener only edits .claude.json (unset it, or set the trust field in that file yourself)",
			ErrUnsupportedConfig, filepath.Join(base, ".claude-custom-oauth.json"))
	}
	legacy := filepath.Join(claudeHome, ".config.json")
	if fi, err := os.Stat(legacy); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		return "", fmt.Errorf("%w: %s exists and is not empty, so Claude Code may use it instead of %s; fastener will not guess (set the trust field in the file Claude uses yourself)",
			ErrUnsupportedConfig, legacy, filepath.Join(base, ".claude.json"))
	}
	return filepath.Join(base, ".claude.json"), nil
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
func Trust(key string, o Options) (res Result, err error) {
	res = Result{Key: key, ConfigPath: o.ConfigPath}
	if o.ConfigPath == "" || o.BackupDir == "" {
		return res, errors.New("claudetrust: ConfigPath and BackupDir are required")
	}
	if o.LockTimeout <= 0 {
		o.LockTimeout = DefaultLockTimeout
	}
	if o.LockStale <= 0 {
		o.LockStale = DefaultLockStale
	}
	// The write goes through to the real file, so a symlinked config stays a
	// symlink.
	real, err := filepath.EvalSymlinks(o.ConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%s: %w", o.ConfigPath, ErrNoClaudeConfig)
	}
	if err != nil {
		return res, err
	}
	// Claude 2.1.319 locks "<config path>.lock" on the path as it computes
	// it, not resolved through symlinks. When the config is a symlink, the
	// lock on the real path (proper-lockfile's default) is taken too, after
	// it, for other writers that lock there.
	paths := []string{o.ConfigPath + ".lock"}
	if real != filepath.Clean(o.ConfigPath) {
		paths = append(paths, real+".lock")
	}
	locks, err := acquireLocks(paths, o.LockTimeout, o.LockStale)
	if err != nil {
		return res, err
	}
	defer func() { // runs on a panic too
		if rerr := locks.release(); rerr != nil && err == nil {
			err = fmt.Errorf("releasing the lock: %w", rerr)
		}
	}()
	changed, backup, err := trustLocked(real, key, o.BackupDir, locks.check)
	res.AlreadyTrusted, res.Backup = !changed, backup
	if err != nil {
		return res, err
	}
	return res, Verify(o.ConfigPath, key)
}

// trustLocked does the read-modify-write; the caller holds the lock, and
// stillHeld is checked right before the rename.
func trustLocked(path, key, backupDir string, stillHeld func() error) (changed bool, backup string, err error) {
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
	var added []string // object keys this edit adds, as written
	projects := newObject()
	if v, ok := root.get("projects"); ok {
		if projects, ok = v.(*object); !ok {
			return false, "", fmt.Errorf("%s: projects is not a JSON object", path)
		}
	} else {
		added = append(added, quote("projects"))
	}
	entry := newObject()
	if v, ok := projects.get(key); ok {
		if entry, ok = v.(*object); !ok {
			return false, "", fmt.Errorf("%s: projects[%q] is not a JSON object", path, key)
		}
	} else {
		added = append(added, quote(key))
	}
	if v, ok := entry.get(TrustField); ok {
		if raw, ok := v.(json.RawMessage); ok && string(raw) == "true" {
			return false, "", nil
		}
	} else {
		added = append(added, quote(TrustField))
	}
	entry.set(TrustField, json.RawMessage("true"))
	projects.set(key, entry)
	root.set("projects", projects)
	out := encodeStringify(root)
	if bytes.HasSuffix(orig, []byte("\n")) {
		out = append(out, '\n')
	}
	if err := checkOnlyTrustChanged(orig, out, key, added); err != nil {
		return false, "", fmt.Errorf("%s: refusing to write: %w", path, err)
	}
	if backup, err = writeBackup(backupDir, orig); err != nil {
		return false, "", fmt.Errorf("backing up %s: %w", path, err)
	}
	if err := writeAtomic(path, out, fi.Mode().Perm(), stillHeld); err != nil {
		return false, backup, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, backup, nil
}

func quote(s string) string {
	var b bytes.Buffer
	writeString(&b, s)
	return b.String()
}

// writeAtomic writes data to a temp file in path's directory, fsyncs it,
// runs beforeRename (which can abort the write) and renames it over path
// with mode perm. The temp file is removed on every failure.
func writeAtomic(path string, data []byte, perm fs.FileMode, beforeRename func() error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return fmt.Errorf("not replacing the file: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	renamed = true
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync() // best effort; some filesystems reject directory fsync
		dir.Close()
	}
	return nil
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

// checkOnlyTrustChanged checks, independently of the ordered codec, that out
// equals orig with only projects[key].hasTrustDialogAccepted set to true:
// with encoding/json the values must match, and with a byte-level scan the
// object keys must be orig's, byte for byte, plus exactly the ones added.
// (encoding/json alone would miss keys that it decodes lossily, such as two
// keys differing only in lone-surrogate escapes, merging into one.)
func checkOnlyTrustChanged(orig, out []byte, key string, added []string) error {
	if err := sameKeysPlus(orig, out, added); err != nil {
		return err
	}
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
	if err := writeAtomic(name, b, 0o600, nil); err != nil {
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

// objectKeys returns the source bytes (with quotes) of every object key in
// a JSON document, found by a plain byte scan: a string token followed,
// after optional whitespace, by ':'.
func objectKeys(b []byte) []string {
	var keys []string
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		start := i
		for i++; i < len(b) && b[i] != '"'; i++ {
			if b[i] == '\\' {
				i++
			}
		}
		end := i + 1
		j := end
		for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
			j++
		}
		if j < len(b) && b[j] == ':' && end <= len(b) {
			keys = append(keys, string(b[start:end]))
		}
	}
	return keys
}

// sameKeysPlus checks out's object keys are orig's plus added (as multisets).
func sameKeysPlus(orig, out []byte, added []string) error {
	count := map[string]int{}
	for _, k := range objectKeys(orig) {
		count[k]++
	}
	for _, k := range added {
		count[k]++
	}
	for _, k := range objectKeys(out) {
		count[k]--
	}
	for k, n := range count {
		if n != 0 {
			return fmt.Errorf("the re-encoded file would change the object key %s (count off by %d)", k, n)
		}
	}
	return nil
}
