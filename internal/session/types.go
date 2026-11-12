// Package session owns Fastener's on-disk state: one directory per session,
// plain JSON files, atomic writes, and the rules for who writes what.
package session

import (
	"encoding/json"
	"time"

	"github.com/ytjohn/fastener/internal/parser"
	"github.com/ytjohn/fastener/internal/proc"
)

// SchemaVersion is written into every JSON file. Readers accept this and all
// earlier versions; fields are only ever added, never repurposed.
const SchemaVersion = 1

const (
	StateStarting      = "starting"
	StateRunning       = "running"
	StateExited        = "exited"
	StateLost          = "lost" // inferred on read; never written by the monitor
	StateRestoreFailed = "restore-failed"
)

const (
	EndedByExit         = "exit"
	EndedByLimit        = "limit"
	EndedByStop         = "stop"
	EndedBySignal       = "signal"
	EndedByLaunchFailed = "launch-failed"
)

// Session kinds (session.json "kind").
const (
	KindHeadless = "headless"
	KindRemote   = "remote" // a remote bridge (run --remote)
	// KindInteractive is run --interactive (with or without --remote): the
	// agent's TUI on a monitor-owned pty.
	KindInteractive = "interactive"
)

// Duration serialises as a Go duration string ("40m").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = 0
		return nil
	}
	var n int64
	if json.Unmarshal(b, &n) == nil {
		*d = Duration(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Session is the caller's intent. Written once by the CLI; never modified.
type Session struct {
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	CreatedAt     time.Time         `json:"created_at"`
	Kind          string            `json:"kind"`
	Backend       string            `json:"backend"`
	Cwd           string            `json:"cwd"`
	PromptSource  string            `json:"prompt_source"` // "" for a remote bridge
	Permissions   string            `json:"permissions"`
	Model         string            `json:"model,omitempty"`
	MaxRuntime    Duration          `json:"max_runtime,omitempty"`
	Grace         Duration          `json:"grace"`
	EnvBase       []string          `json:"env_base,omitempty"`
	Env           []string          `json:"env,omitempty"`
	EnvFile       string            `json:"env_file,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Restart       string            `json:"restart"`
	Remote        bool              `json:"remote"`
	// Adopted marks an interactive session made by `fastener adopt`: its
	// first generation resumed a conversation started outside Fastener
	// (the plan's session_id). Absent for every other session.
	Adopted bool `json:"adopted,omitempty"`
}

// DefaultGrace is the stop grace for a session that recorded none.
const DefaultGrace = 10 * time.Second

// EffectiveGrace is the session's grace, else DefaultGrace. Both stop and the
// systemd launcher's unit timers use it.
func (s Session) EffectiveGrace() time.Duration {
	if g := time.Duration(s.Grace); g > 0 {
		return g
	}
	return DefaultGrace
}

// Plan is one generation's launch plan, written before the monitor starts.
// The monitor reads only this and never opens the profile.
type Plan struct {
	SchemaVersion int      `json:"schema_version"`
	Generation    int      `json:"generation"`
	Argv          []string `json:"argv"`
	PromptMode    string   `json:"prompt_mode"`
	Parser        string   `json:"parser"`
	// DedupeFrames: drop redrawn lines from output.log (still parsed); from
	// the profile's [remote] dedupe_frames.
	DedupeFrames bool `json:"dedupe_frames,omitempty"`
	// PTY, when set, makes this an interactive generation: the monitor runs
	// the agent on a pseudo-terminal it owns and serves attach.sock (design
	// §8.2). Absent in every plan written before v0.4, which run exactly as
	// before.
	PTY *PTYPlan `json:"pty,omitempty"`
	// URLPattern, when set, is matched against the agent's terminal output
	// (escapes stripped); the latest match is status remote_url. From the
	// profile's [interactive.remote] url_pattern.
	URLPattern string `json:"url_pattern,omitempty"`
	// Env is KEY=VALUE pairs from the profile ([interactive].env), layered
	// after the session's env_base and before its env-file and --env.
	Env []string `json:"env,omitempty"`
	// SessionID is the conversation ID passed to the agent at launch (the
	// profile's [interactive] session_id); Finder names the finder that
	// locates the conversation (internal/finder), for status
	// conversation_id. Interactive generations only.
	SessionID       string    `json:"session_id,omitempty"`
	Finder          string    `json:"finder,omitempty"`
	ProfileSource   string    `json:"profile_source"`
	ProfileSHA256   string    `json:"profile_sha256"`
	Marker          string    `json:"marker"`
	FastenerVersion string    `json:"fastener_version"`
	PlannedAt       time.Time `json:"planned_at"`
}

// PTYPlan is the pty of an interactive generation.
type PTYPlan struct {
	// Rows and Cols are the initial window size; 0 means the default
	// (DefaultPTYRows × DefaultPTYCols). The first client to attach with a
	// size replaces it.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// The pty size used when a plan gives none.
const (
	DefaultPTYRows = 24
	DefaultPTYCols = 80
)

// Size is the initial size with the defaults applied.
func (p PTYPlan) Size() (rows, cols uint16) {
	rows, cols = p.Rows, p.Cols
	if rows == 0 {
		rows = DefaultPTYRows
	}
	if cols == 0 {
		cols = DefaultPTYCols
	}
	return rows, cols
}

// LauncherRecord identifies the monitor before it has written anything.
type LauncherRecord struct {
	SchemaVersion int            `json:"schema_version"`
	Launcher      string         `json:"launcher"`
	Unit          string         `json:"unit,omitempty"`
	Monitor       *proc.Identity `json:"monitor,omitempty"`
	BootID        string         `json:"boot_id"`
	LaunchedAt    time.Time      `json:"launched_at"`
}

// Status holds runtime facts. Only the monitor writes it while it is alive.
type Status struct {
	SchemaVersion int            `json:"schema_version"`
	State         string         `json:"state"`
	Generation    int            `json:"generation"`
	Monitor       *proc.Identity `json:"monitor,omitempty"`
	Agent         *proc.Identity `json:"agent,omitempty"`
	PGID          int            `json:"pgid,omitempty"`
	BootID        string         `json:"boot_id,omitempty"`
	StartedAt     *time.Time     `json:"started_at,omitempty"`
	EndedAt       *time.Time     `json:"ended_at,omitempty"`
	EndedBy       string         `json:"ended_by,omitempty"`
	ExitCode      *int           `json:"exit_code,omitempty"`
	Signal        string         `json:"signal,omitempty"`
	Error         string         `json:"error,omitempty"`
	// OutputTruncated: the agent exited but its stdout was still held open
	// after the drain timeout, so anything written later is not in output.log.
	OutputTruncated bool `json:"output_truncated,omitempty"`
	// RestoreReason is set with state restore-failed: why `fastener restore`
	// could not relaunch the session (a code such as no_remote or
	// untrusted_workspace; Error has the message).
	RestoreReason string `json:"restore_reason,omitempty"`
	// AttachSocket is where the monitor of an interactive (pty) generation
	// listens for attach clients, and AttachedClients how many are attached
	// (flushed about once a second). Both are set only while it runs.
	AttachSocket    string `json:"attach_socket,omitempty"`
	AttachedClients *int   `json:"attached_clients,omitempty"`
	// For an interactive session: ConversationID (parser.Fields) is set only
	// from an exact source, the ID assigned at launch once the agent has
	// written that conversation (ConversationSource "session_id").
	// ConversationCandidate is a heuristic finder's answer
	// (ConversationSource is then the finder's name): possibly another
	// conversation in the same folder, so a restore uses it only when told
	// to. ConversationError says why there is neither: nothing written yet,
	// or more than one candidate.
	ConversationCandidate string `json:"conversation_candidate,omitempty"`
	ConversationSource    string `json:"conversation_source,omitempty"`
	ConversationError     string `json:"conversation_error,omitempty"`
	parser.Fields
	UpdatedAt time.Time `json:"updated_at"`
}

// StopRequest tells the monitor a SIGTERM came from Fastener.
type StopRequest struct {
	SchemaVersion int       `json:"schema_version"`
	Generation    int       `json:"generation"`
	RequestedAt   time.Time `json:"requested_at"`
	Grace         Duration  `json:"grace"`
	By            string    `json:"by"`
}
