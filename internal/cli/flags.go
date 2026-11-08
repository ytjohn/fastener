package cli

import (
	"encoding/json"
	"flag"
	"io"
	"strings"
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
// ("run claude --cwd x" and "run --cwd x claude" both work).
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
		pos = append(pos, rest[0])
		args = rest[1:]
	}
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
