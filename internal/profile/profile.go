// Package profile loads backend profiles: TOML files that describe how to
// invoke an agent CLI. User files in the backends directory replace embedded
// built-ins of the same name entirely (no merging).
//
// Profile files are trusted configuration: a profile controls both the command
// that runs and the permission flags it receives, so the backends directory
// must be writable only by the user.
//
// check.version_args is optional; when it is empty, check's binary step only
// verifies that the command can be looked up.
//
// default_permissions is optional; when set it must name a level the profile
// defines, and it applies only when neither --permissions nor the config's
// default_permissions is given (see ResolvePermissions).
//
// Names lists every *.toml file it finds, including broken ones, so that
// `check --all` surfaces them rather than hiding them.
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

// ErrNoPermissionLevel is wrapped by RenderHeadless when the profile does not
// define the requested permission level.
var ErrNoPermissionLevel = errors.New("permission level not defined")

// noLevelError carries the message alone and matches ErrNoPermissionLevel via Is,
// so the sentinel's text is not repeated in the output.
type noLevelError struct{ msg string }

func (e noLevelError) Error() string        { return e.msg }
func (e noLevelError) Is(target error) bool { return target == ErrNoPermissionLevel }

// KnownLevels are the permission levels a profile may define.
func KnownLevels() []string { return []string{"auto", "skip"} }

var levels = map[string]bool{"auto": true, "skip": true}
var options = map[string]bool{"model": true}
var commandRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type Profile struct {
	SchemaVersion int    `toml:"schema_version"`
	Name          string `toml:"name"`
	Command       string `toml:"command"`
	// DefaultPermissions is the level used when neither the flag nor the
	// config names one. It must be a level this profile defines.
	DefaultPermissions string              `toml:"default_permissions"`
	Headless           Headless            `toml:"headless"`
	Options            map[string][]string `toml:"options"`
	Permissions        map[string][]string `toml:"permissions"`
	Resume             *Resume             `toml:"resume"`
	Remote             *Remote             `toml:"remote"`
	Check              Check               `toml:"check"`

	Source string `toml:"-"` // "builtin" or the file path
	SHA256 string `toml:"-"` // of the file's bytes
}

type Headless struct {
	Args   []string `toml:"args"`
	Prompt string   `toml:"prompt"` // "stdin" | "arg"
	Parser string   `toml:"parser"`
}

// Remote describes a remote bridge (`run --remote`): a long-running process
// that is driven from elsewhere, so it takes no prompt.
type Remote struct {
	// Args follow the command; {name} (at most once) is the session name.
	Args []string `toml:"args"`
	// Parser defaults to "claude-remote".
	Parser string `toml:"parser"`
	// Permissions overrides [permissions] per level, for a bridge command
	// that rejects the headless flags. A level not listed here uses
	// [permissions]; a level listed here must be defined there too.
	Permissions map[string][]string `toml:"permissions"`
	// UntrustedPattern is a regex matched against stderr.log of a bridge
	// that exits before reporting its URL; a match means the folder is not a
	// trusted workspace for the backend.
	UntrustedPattern string `toml:"untrusted_pattern"`
	// UntrustedHint, optional, tells the user how to trust the folder; {dir}
	// is replaced by the folder. Without it the error names no command.
	UntrustedHint string `toml:"untrusted_hint"`
	// DedupeFrames makes the monitor drop redrawn status lines from
	// output.log (they are still parsed).
	DedupeFrames bool `toml:"dedupe_frames"`
}

// DefaultRemoteParser is [remote].parser when the profile names none.
const DefaultRemoteParser = "claude-remote"

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
	if err := validateCommand(p.Command); err != nil {
		return err
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
	if d := p.DefaultPermissions; d != "" {
		if _, ok := p.Permissions[d]; !ok {
			return fmt.Errorf("default_permissions %q is not a level this profile defines (defined: %s)", d, strings.Join(p.Levels(), ", "))
		}
	}
	for opt := range p.Options {
		if !options[opt] {
			return fmt.Errorf("options.%s: unknown option (known: model)", opt)
		}
	}
	if err := p.validateTemplates(); err != nil {
		return err
	}
	if err := p.validateRemote(); err != nil {
		return err
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

// ResolvePermissions picks the permission level for a run: the --permissions
// flag, else the config's default_permissions, else the profile's
// default_permissions, else "auto". The result may still be undefined for this
// profile; RenderHeadless rejects it then.
func (p *Profile) ResolvePermissions(flag, configDefault string) string {
	for _, l := range []string{flag, configDefault, p.DefaultPermissions} {
		if l != "" {
			return l
		}
	}
	return "auto"
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

func validateCommand(c string) error {
	if strings.ContainsAny(c, " \t\r\n") {
		return fmt.Errorf("command %q must not contain whitespace", c)
	}
	if filepath.IsAbs(c) || commandRE.MatchString(c) {
		return nil
	}
	return fmt.Errorf("command %q must be a bare name or an absolute path", c)
}

const namePH = "{name}"

func (p *Profile) validateRemote() error {
	r := p.Remote
	if r == nil {
		return nil
	}
	if len(r.Args) == 0 {
		return errors.New("remote.args must not be empty")
	}
	if countIn(r.Args, namePH) > 1 {
		return fmt.Errorf("remote.args must contain %s at most once", namePH)
	}
	if _, ok := parser.Get(r.RemoteParser()); !ok {
		return fmt.Errorf("remote.parser %q unknown (known: %s)", r.Parser, strings.Join(parser.Names(), ", "))
	}
	for lvl := range r.Permissions {
		if !levels[lvl] {
			return fmt.Errorf("remote.permissions.%s: unknown level (known: auto, skip)", lvl)
		}
		if _, ok := p.Permissions[lvl]; !ok {
			return fmt.Errorf("remote.permissions.%s: level is not defined in [permissions]", lvl)
		}
	}
	if r.UntrustedHint != "" && r.UntrustedPattern == "" {
		return errors.New("remote.untrusted_hint needs remote.untrusted_pattern")
	}
	if r.UntrustedPattern != "" {
		if _, err := regexp.Compile(r.UntrustedPattern); err != nil {
			return fmt.Errorf("remote.untrusted_pattern: %w", err)
		}
	}
	return nil
}

// RemoteParser is the parser a remote bridge uses.
func (r *Remote) RemoteParser() string {
	if r.Parser == "" {
		return DefaultRemoteParser
	}
	return r.Parser
}

func countIn(args []string, tok string) int {
	n := 0
	for _, a := range args {
		n += strings.Count(a, tok)
	}
	return n
}

func (p *Profile) validateTemplates() error {
	const val, conv = "{value}", "{conversation_id}"
	var plain [][]string // argument lists where no placeholder may appear
	plain = append(plain, p.Headless.Args, p.Check.VersionArgs)
	for _, a := range p.Permissions {
		plain = append(plain, a)
	}
	if p.Remote != nil {
		for _, a := range p.Remote.Permissions {
			plain = append(plain, a)
		}
	}
	// {name} is only allowed in [remote].args (see validateRemote).
	noName := append([][]string{}, plain...)
	for _, t := range p.Options {
		noName = append(noName, t)
	}
	if p.Resume != nil {
		noName = append(noName, p.Resume.Args)
	}
	for _, a := range noName {
		if countIn(a, namePH) != 0 {
			return fmt.Errorf("placeholder %s is only allowed in remote.args", namePH)
		}
	}
	if p.Remote != nil {
		plain = append(plain, p.Remote.Args)
	}
	for name, tmpl := range p.Options {
		if len(tmpl) == 0 || countIn(tmpl, val) != 1 {
			return fmt.Errorf("options.%s must be non-empty and contain %s exactly once", name, val)
		}
		if countIn(tmpl, conv) != 0 {
			return fmt.Errorf("options.%s must not contain %s", name, conv)
		}
	}
	if p.Resume != nil {
		if len(p.Resume.Args) == 0 || countIn(p.Resume.Args, conv) != 1 {
			return fmt.Errorf("resume.args must be non-empty and contain %s exactly once", conv)
		}
		if countIn(p.Resume.Args, val) != 0 {
			return fmt.Errorf("resume.args must not contain %s", val)
		}
	}
	for _, a := range plain {
		if countIn(a, val) != 0 || countIn(a, conv) != 0 {
			return fmt.Errorf("placeholders %s and %s are only allowed in options and resume templates", val, conv)
		}
	}
	return nil
}
