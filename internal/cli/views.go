package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/session"
)

// JSONSchemaVersion versions the --json output contract (docs/json-schema.md).
const JSONSchemaVersion = 1

type pathsOut struct {
	Dir        string `json:"dir"`
	OutputLog  string `json:"output_log"`
	StderrLog  string `json:"stderr_log"`
	MonitorLog string `json:"monitor_log"`
}

type inspectOut struct {
	SchemaVersion int                     `json:"schema_version"`
	Name          string                  `json:"name"`
	State         string                  `json:"state"`
	Backend       string                  `json:"backend"`
	Orphans       int                     `json:"orphans"`
	Session       session.Session         `json:"session"`
	Status        session.Status          `json:"status"`
	Plan          *session.Plan           `json:"plan,omitempty"`
	Launcher      *session.LauncherRecord `json:"launcher,omitempty"`
	Paths         pathsOut                `json:"paths"`
	Warnings      []string                `json:"warnings,omitempty"`
	// LivenessError is set while the session is starting or running and the
	// monitor's liveness could not be determined (it is then assumed alive).
	LivenessError string `json:"liveness_error,omitempty"`
	// OrphanError is set when the orphan count cannot be trusted (no readable
	// plan, a failed process scan, or a reload that failed after launch).
	OrphanError string `json:"orphan_error,omitempty"`
	// Reason and Folder are set only by `run --remote` when the bridge ended
	// before reporting its URL: reason "untrusted_workspace" (with the folder)
	// or "exited".
	Reason string `json:"reason,omitempty"`
	Folder string `json:"folder,omitempty"`
}

func inspectOf(v *session.View) inspectOut {
	return inspectOut{
		SchemaVersion: JSONSchemaVersion, Name: v.Session.Name, State: v.State, Backend: v.Session.Backend,
		Orphans: len(v.Orphans), Session: v.Session, Status: v.Status, Plan: v.Plan, Launcher: v.Launcher,
		Warnings: v.Warnings, LivenessError: v.LivenessError, OrphanError: v.OrphanErr,
		Paths: pathsOut{Dir: string(v.Dir), OutputLog: v.Dir.OutputLog(), StderrLog: v.Dir.StderrLog(), MonitorLog: v.Dir.MonitorLog()},
	}
}

type psRow struct {
	Name      string            `json:"name"`
	Backend   string            `json:"backend"`
	State     string            `json:"state"`
	CreatedAt time.Time         `json:"created_at"`
	EndedAt   *time.Time        `json:"ended_at,omitempty"`
	EndedBy   string            `json:"ended_by,omitempty"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	// RemoteURL is a remote bridge's URL, once it has reported one.
	RemoteURL string `json:"remote_url,omitempty"`
}

func handshakeTimeout() time.Duration {
	cfg, err := config.Load()
	if err != nil {
		return 5 * time.Second
	}
	return cfg.Handshake()
}

// loadView includes the orphan scan (inspect, stop, rm, wait, logs); ps uses cheap Load.
func loadView(name string) (*session.View, error) { return session.LoadFull(name, handshakeTimeout()) }

func oneName(pos []string, stderr io.Writer, usage string) (string, bool) {
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: fastener "+usage)
		return "", false
	}
	return pos[0], true
}

func cmdInspect(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("inspect <name>", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	name, ok := oneName(pos, stderr, "inspect <name> [--json]")
	if !ok {
		return exitUsage
	}
	v, err := loadView(name)
	if err != nil {
		return fail(stderr, err)
	}
	out := inspectOf(v)
	if *asJSON {
		writeJSON(stdout, out) // warnings travel in the "warnings" array
		return exitOK
	}
	for _, w := range v.Warnings {
		fmt.Fprintf(stderr, "fastener: warning: %s: %s\n", name, w)
	}
	renderInspect(stdout, v, out)
	return exitOK
}

// renderInspect writes the human-readable inspect output.
func renderInspect(stdout io.Writer, v *session.View, out inspectOut) {
	s := v.Status
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	row := func(k, val string) {
		if val != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, val)
		}
	}
	row("name", out.Name)
	row("backend", out.Backend)
	row("state", out.State)
	if v.OrphanErr != "" {
		row("orphans", "unknown ("+v.OrphanErr+")")
	}
	if v.LivenessError != "" {
		row("liveness", "unknown ("+v.LivenessError+")")
	}
	row("cwd", v.Session.Cwd)
	row("permissions", v.Session.Permissions)
	row("created", v.Session.CreatedAt.Local().Format(time.RFC3339))
	if s.EndedAt != nil {
		row("ended", s.EndedAt.Local().Format(time.RFC3339))
	}
	row("ended_by", s.EndedBy)
	if s.ExitCode != nil {
		row("exit_code", fmt.Sprint(*s.ExitCode))
	}
	row("signal", s.Signal)
	row("error", s.Error)
	row("conversation_id", s.ConversationID)
	row("final_message", truncate(s.FinalMessage, 200))
	row("served_model", s.Model)
	if s.AgentError != nil {
		row("agent_error", fmt.Sprint(*s.AgentError))
	}
	row("remote_url", s.RemoteURL)
	for _, u := range s.RemoteSessions {
		row("remote_session", u)
	}
	if len(v.Orphans) > 0 {
		row("orphans", fmt.Sprint(len(v.Orphans)))
	}
	row("output_log", out.Paths.OutputLog)
	row("stderr_log", out.Paths.StderrLog)
	tw.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func cmdPs(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ps", stderr)
	all := fs.Bool("all", false, "include everything retained, not just recent")
	asJSON := fs.Bool("json", false, "print JSON")
	var filterArgs multiFlag
	fs.Var(&filterArgs, "filter", "keep sessions matching KEY=VALUE (label=K[=V], state=S, backend=B, name=GLOB); repeatable, ANDed")
	if _, err := parseArgs(fs, args); err != nil {
		return parseFail(err)
	}
	filters, err := parsePsFilters(filterArgs)
	if err != nil {
		return fail(stderr, err)
	}
	autoPrune() // before listing, so ps never shows what it is about to remove
	names, err := session.Names()
	if err != nil {
		return fail(stderr, err)
	}
	hs := handshakeTimeout()
	cutoff := time.Now().Add(-24 * time.Hour)
	rows := []psRow{}
	for _, n := range names {
		v, err := session.Load(n, hs)
		if err != nil {
			fmt.Fprintf(stderr, "fastener: skipping %s: %v\n", n, err)
			continue
		}
		for _, w := range v.Warnings {
			fmt.Fprintf(stderr, "fastener: warning: %s: %s\n", n, w)
		}
		recent := v.Alive() || v.State == session.StateLost ||
			(v.Status.EndedAt != nil && v.Status.EndedAt.After(cutoff)) ||
			(v.Status.EndedAt == nil && v.Session.CreatedAt.After(cutoff))
		if !*all && !recent {
			continue
		}
		row := psRow{
			Name: n, Backend: v.Session.Backend, State: v.State, CreatedAt: v.Session.CreatedAt,
			EndedAt: v.Status.EndedAt, EndedBy: v.Status.EndedBy, ExitCode: v.Status.ExitCode,
			Labels: v.Session.Labels, RemoteURL: v.Status.RemoteURL,
		}
		if matchAll(filters, row) {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
	if *asJSON {
		writeJSON(stdout, rows)
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBACKEND\tSTATE\tAGE\tENDED_BY\tEXIT")
	for _, r := range rows {
		exit := ""
		if r.ExitCode != nil {
			exit = fmt.Sprint(*r.ExitCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Backend, r.State,
			time.Since(r.CreatedAt).Round(time.Second), r.EndedBy, exit)
	}
	tw.Flush()
	return exitOK
}
