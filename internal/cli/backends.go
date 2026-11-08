package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/profile"
	"github.com/ytjohn/fastener/internal/version"
)

type backendOut struct {
	Name        string   `json:"name"`
	Source      string   `json:"source,omitempty"`
	SHA256      string   `json:"sha256,omitempty"`
	Command     string   `json:"command,omitempty"`
	Parser      string   `json:"parser,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	// DefaultPermissions is the profile's own default level, if it sets one.
	DefaultPermissions string `json:"default_permissions,omitempty"`
	Error              string `json:"error,omitempty"`
}

func cmdBackends(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("backends", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return parseFail(err)
	}
	names, err := profile.Names(config.BackendsDir())
	if err != nil {
		return fail(stderr, err)
	}
	out := []backendOut{}
	for _, n := range names {
		p, err := profile.Load(n, config.BackendsDir())
		if err != nil {
			out = append(out, backendOut{Name: n, Error: err.Error()})
			continue
		}
		out = append(out, backendOut{Name: n, Source: p.Source, SHA256: p.SHA256, Command: p.Command,
			Parser: p.Headless.Parser, Permissions: p.Levels(), DefaultPermissions: p.DefaultPermissions})
	}
	if *asJSON {
		writeJSON(stdout, out)
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tPARSER\tPERMISSIONS\tDEFAULT")
	for _, b := range out {
		if b.Error != "" {
			fmt.Fprintf(tw, "%s\tERROR: %s\t\t\t\n", b.Name, b.Error)
			continue
		}
		def := b.DefaultPermissions
		if def == "" {
			def = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", b.Name, b.Source, b.Parser, b.Permissions, def)
	}
	tw.Flush()
	return exitOK
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return parseFail(err)
	}
	if *asJSON {
		writeJSON(stdout, map[string]any{"schema_version": JSONSchemaVersion, "version": version.Version})
		return exitOK
	}
	fmt.Fprintln(stdout, "fastener", version.Version)
	return exitOK
}
