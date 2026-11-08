// Package profile loads backend profiles: TOML files that describe how to
// invoke an agent CLI. User files in the backends directory replace embedded
// built-ins of the same name entirely (no merging).
package profile

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/ytjohn/fastener/internal/parser"
)

//go:embed profiles/*.toml
var builtin embed.FS

var ErrUnknownBackend = errors.New("unknown backend")

var levels = map[string]bool{"auto": true, "skip": true}
var options = map[string]bool{"model": true}
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type Profile struct {
	SchemaVersion int                 `toml:"schema_version"`
	Name          string              `toml:"name"`
	Command       string              `toml:"command"`
	Headless      Headless            `toml:"headless"`
	Options       map[string][]string `toml:"options"`
	Permissions   map[string][]string `toml:"permissions"`
	Resume        *Resume             `toml:"resume"`
	Check         Check               `toml:"check"`

	Source string `toml:"-"` // "builtin" or the file path
	SHA256 string `toml:"-"` // of the file's bytes
}

type Headless struct {
	Args   []string `toml:"args"`
	Prompt string   `toml:"prompt"` // "stdin" | "arg"
	Parser string   `toml:"parser"`
}

type Resume struct {
	Args []string `toml:"args"`
}

type Check struct {
	VersionArgs []string    `toml:"version_args"`
	Diagnose    []Diagnosis `toml:"diagnose"`
}

type Diagnosis struct {
	Match string `toml:"match"`
	Hint  string `toml:"hint"`
}

// Load resolves name: <userDir>/<name>.toml, else the embedded built-in.
func Load(name, userDir string) (*Profile, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, name)
	}
	var data []byte
	var source string
	if userDir != "" {
		path := filepath.Join(userDir, name+".toml")
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			data, source = b, path
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	if data == nil {
		b, err := builtin.ReadFile("profiles/" + name + ".toml")
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, name)
		}
		data, source = b, "builtin"
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("backend %s (%s): %w", name, source, err)
	}
	if p.Name != name {
		return nil, fmt.Errorf("backend %s (%s): name field is %q", name, source, p.Name)
	}
	sum := sha256.Sum256(data)
	p.Source, p.SHA256 = source, hex.EncodeToString(sum[:])
	return p, nil
}

// Parse decodes and validates profile TOML. Unknown keys are errors.
func Parse(data []byte) (*Profile, error) {
	var p Profile
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	return &p, p.validate()
}

func (p *Profile) validate() error {
	if p.SchemaVersion != 1 {
		return fmt.Errorf("schema_version %d not supported (want 1)", p.SchemaVersion)
	}
	if p.Name == "" || p.Command == "" {
		return errors.New("name and command are required")
	}
	if p.Headless.Prompt != "stdin" && p.Headless.Prompt != "arg" {
		return fmt.Errorf("headless.prompt %q: want \"stdin\" or \"arg\"", p.Headless.Prompt)
	}
	if _, ok := parser.Get(p.Headless.Parser); !ok {
		return fmt.Errorf("headless.parser %q unknown (known: %s)", p.Headless.Parser, strings.Join(parser.Names(), ", "))
	}
	if len(p.Permissions) == 0 {
		return errors.New("[permissions] must define at least one level")
	}
	for lvl := range p.Permissions {
		if !levels[lvl] {
			return fmt.Errorf("permissions.%s: unknown level (known: auto, skip)", lvl)
		}
	}
	for opt := range p.Options {
		if !options[opt] {
			return fmt.Errorf("options.%s: unknown option (known: model)", opt)
		}
	}
	for i, d := range p.Check.Diagnose {
		if _, err := regexp.Compile(d.Match); err != nil {
			return fmt.Errorf("check.diagnose[%d].match: %w", i, err)
		}
		if d.Hint == "" {
			return fmt.Errorf("check.diagnose[%d].hint is required", i)
		}
	}
	return nil
}

// Levels lists the permission levels a profile defines, sorted.
func (p *Profile) Levels() []string {
	out := make([]string, 0, len(p.Permissions))
	for l := range p.Permissions {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Names lists every resolvable backend: built-ins plus user files, sorted.
func Names(userDir string) ([]string, error) {
	set := map[string]bool{}
	ents, err := builtin.ReadDir("profiles")
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		set[strings.TrimSuffix(e.Name(), ".toml")] = true
	}
	if userDir != "" {
		files, err := os.ReadDir(userDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, f := range files {
			if n := strings.TrimSuffix(f.Name(), ".toml"); n != f.Name() && nameRE.MatchString(n) {
				set[n] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
