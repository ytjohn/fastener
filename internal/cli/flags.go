package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/profile"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("fastener "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseArgs lets flags and positional arguments interleave
// ("run claude --cwd x" and "run --cwd x claude" both work). A bare "--" ends
// flag parsing: everything after it is positional, even if it starts with "-".
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" && !isFlagValue(fs, args, consumed-1) {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// isFlagValue reports whether args[i] was consumed as the value of the value
// flag at args[i-1] (as in "--cwd --") rather than as the "--" terminator.
func isFlagValue(fs *flag.FlagSet, args []string, i int) bool {
	if i == 0 {
		return false
	}
	prev := args[i-1]
	if !strings.HasPrefix(prev, "-") || strings.Contains(prev, "=") {
		return false
	}
	f := fs.Lookup(strings.TrimLeft(prev, "-"))
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}

func parseLabels(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || k == "" {
			return nil, usagef("--label %q: want KEY=VALUE", it)
		}
		out[k] = v
	}
	return out, nil
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// parseFail maps a flag-parse error to an exit code: -h/--help is success.
func parseFail(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitUsage
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnv checks --env items (KEY=VALUE with a portable key).
func validateEnv(items []string) error {
	for _, kv := range items {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !envKeyRE.MatchString(k) {
			return usagef("--env %q: want KEY=VALUE with KEY matching %s", kv, envKeyRE)
		}
	}
	return nil
}

// validatePermissions rejects an explicit --permissions outside the known levels.
func validatePermissions(level string) error {
	if level == "" {
		return nil
	}
	for _, k := range profile.KnownLevels() {
		if level == k {
			return nil
		}
	}
	return usagef("--permissions %q: want one of %s", level, strings.Join(profile.KnownLevels(), ", "))
}

// validateDurations rejects negative --grace / --max-runtime values.
func validateDurations(grace, maxRuntime time.Duration) error {
	if grace < 0 {
		return usagef("--grace must not be negative")
	}
	if maxRuntime < 0 {
		return usagef("--max-runtime must not be negative")
	}
	return nil
}
