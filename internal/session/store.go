package session

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var (
	ErrNotFound  = errors.New("no such session")
	ErrNameTaken = errors.New("session name already exists")
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func ValidName(name string) bool { return nameRE.MatchString(name) }

// Root is $FASTENER_STATE_DIR, else $XDG_STATE_HOME/fastener, else ~/.local/state/fastener.
// XDG_STATE_HOME is used only if absolute. Root returns "" when no state
// directory can be determined (no HOME); callers must treat that as an error.
func Root() string {
	if v := os.Getenv("FASTENER_STATE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(v) {
		return filepath.Join(v, "fastener")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "fastener")
}

// ErrNoStateDir is returned when Root is empty.
var ErrNoStateDir = errors.New("no state directory: set HOME or FASTENER_STATE_DIR")

// SessionsDir is "" when there is no state directory, never a relative path.
func SessionsDir() string {
	r := Root()
	if r == "" {
		return ""
	}
	return filepath.Join(r, "sessions")
}

// Dir is one session's directory.
type Dir string

// DirFor is "" when there is no state directory, never a relative path.
func DirFor(name string) Dir {
	sd := SessionsDir()
	if sd == "" {
		return ""
	}
	return Dir(filepath.Join(sd, name))
}

func (d Dir) join(p ...string) string { return filepath.Join(append([]string{string(d)}, p...)...) }
func (d Dir) SessionPath() string     { return d.join("session.json") }
func (d Dir) PromptPath() string      { return d.join("prompt.txt") }
func (d Dir) PlanPath(gen int) string { return d.join("generations", strconv.Itoa(gen)+".json") }
func (d Dir) LauncherPath(gen int) string {
	return d.join("generations", strconv.Itoa(gen)+".launcher.json")
}
func (d Dir) StatusPath() string { return d.join("status.json") }
func (d Dir) StopPath() string   { return d.join("stop-request.json") }
func (d Dir) OutputLog() string  { return d.join("output.log") }
func (d Dir) StderrLog() string  { return d.join("stderr.log") }
func (d Dir) MonitorLog() string { return d.join("monitor.log") }
func (d Dir) LockPath() string   { return d.join("lock") }

// Create reserves name atomically: mkdir either succeeds or the name is taken.
func Create(name string) (Dir, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid session name %q (want %s)", name, nameRE)
	}
	if SessionsDir() == "" {
		return "", ErrNoStateDir
	}
	if err := os.MkdirAll(SessionsDir(), 0o700); err != nil {
		return "", err
	}
	d := DirFor(name)
	if err := os.Mkdir(string(d), 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrNameTaken, name)
		}
		return "", err
	}
	if err := os.Mkdir(d.join("generations"), 0o700); err != nil {
		os.Remove(string(d))
		return "", err
	}
	return d, nil
}

// Names lists existing sessions, sorted.
func Names() ([]string, error) {
	if SessionsDir() == "" {
		return nil, ErrNoStateDir
	}
	ents, err := os.ReadDir(SessionsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && ValidName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func randToken(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return b32.EncodeToString(b)
}

// GenerateName returns "<backend>-<6 random base32 chars>".
func GenerateName(backend string) string { return backend + "-" + randToken(4)[:6] }

// NewMarker returns the per-generation process marker "<name>:<gen>:<nonce>".
func NewMarker(name string, gen int) string {
	return name + ":" + strconv.Itoa(gen) + ":" + randToken(10)
}

// WriteJSON writes v atomically (temp file in the same dir, fsync, rename).
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o600)
}

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if perm != 0o600 { // CreateTemp already makes 0600
		if err := f.Chmod(perm); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Best effort: make the rename durable. Errors are ignored because the
	// rename has already succeeded and some filesystems reject directory fsync.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
