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
	PromptSource  string            `json:"prompt_source"`
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
}

// Plan is one generation's launch plan, written before the monitor starts.
// The monitor reads only this and never opens the profile.
type Plan struct {
	SchemaVersion   int       `json:"schema_version"`
	Generation      int       `json:"generation"`
	Argv            []string  `json:"argv"`
	PromptMode      string    `json:"prompt_mode"`
	Parser          string    `json:"parser"`
	ProfileSource   string    `json:"profile_source"`
	ProfileSHA256   string    `json:"profile_sha256"`
	Marker          string    `json:"marker"`
	FastenerVersion string    `json:"fastener_version"`
	PlannedAt       time.Time `json:"planned_at"`
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
