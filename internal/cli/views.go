package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

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
}

func inspectOf(v *session.View) inspectOut {
	return inspectOut{
		SchemaVersion: JSONSchemaVersion, Name: v.Session.Name, State: v.State, Backend: v.Session.Backend,
		Orphans: len(v.Orphans), Session: v.Session, Status: v.Status, Plan: v.Plan, Launcher: v.Launcher,
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
		return exitUsage
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
		writeJSON(stdout, out)
		return exitOK
	}
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
	if s.AgentError != nil {
		row("agent_error", fmt.Sprint(*s.AgentError))
	}
	if len(v.Orphans) > 0 {
		row("orphans", fmt.Sprint(len(v.Orphans)))
	}
	row("output_log", out.Paths.OutputLog)
	row("stderr_log", out.Paths.StderrLog)
	tw.Flush()
	return exitOK
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func cmdPs(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ps", stderr)
	all := fs.Bool("all", false, "include everything retained, not just recent")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return exitUsage
	}
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
		recent := v.Alive() || v.State == session.StateLost ||
			(v.Status.EndedAt != nil && v.Status.EndedAt.After(cutoff)) ||
			(v.Status.EndedAt == nil && v.Session.CreatedAt.After(cutoff))
		if !*all && !recent {
			continue
		}
		rows = append(rows, psRow{
			Name: n, Backend: v.Session.Backend, State: v.State, CreatedAt: v.Session.CreatedAt,
			EndedAt: v.Status.EndedAt, EndedBy: v.Status.EndedBy, ExitCode: v.Status.ExitCode,
			Labels: v.Session.Labels,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
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
