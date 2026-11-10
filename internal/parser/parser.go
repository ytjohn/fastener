// Package parser extracts optional, mechanical facts from an agent's stdout,
// one line at a time. Parsers never affect the session: a bad line only
// increments ParseErrors.
package parser

import "sort"

// Fields are written into status.json; every field is optional.
type Fields struct {
	ConversationID string `json:"conversation_id,omitempty"`
	FinalMessage   string `json:"final_message,omitempty"`
	// Model is the model the backend reports it actually served: "provider/model"
	// when the backend names the provider, else the bare model id.
	Model string `json:"model,omitempty"`
	// ModelSource says who reported Model: "server" (the API response named
	// it) or "client" (the agent CLI recorded the id it requested, so a
	// provider-side substitution would not show). Empty when Model is.
	ModelSource string `json:"model_source,omitempty"`
	AgentError  *bool  `json:"agent_error,omitempty"`
	Usage       *Usage `json:"usage,omitempty"`
	ParseErrors int    `json:"parse_errors,omitempty"`
	// RemoteURL is a remote bridge's URL (claude-remote: the latest
	// https://claude.ai/code?environment=env_… seen).
	RemoteURL string `json:"remote_url,omitempty"`
	// RemoteSessions lists the session URLs a bridge has shown, de-duplicated,
	// in the order first seen, without their query string.
	RemoteSessions []string `json:"remote_sessions,omitempty"`
}

// Usage counts all tokens the backend reported for the run. The token fields
// are nil when the backend reported none (for example a result carrying only
// a cost), so they are omitted from JSON rather than faked as zero.
type Usage struct {
	// InputTokens is all input tokens for the run, including cache reads and
	// writes (differs from Anthropic's usage.input_tokens, which excludes cache).
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	// CostUSD is nil when the backend reported no cost, so it is omitted
	// rather than claiming a zero cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// equal reports whether a and b hold the same values (not the same pointers).
func (a Usage) equal(b Usage) bool {
	return ptrEq(a.InputTokens, b.InputTokens) && ptrEq(a.OutputTokens, b.OutputTokens) && fptrEq(a.CostUSD, b.CostUSD)
}

func ptrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fptrEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func i64(v int64) *int64 { return &v }

// Parser consumes one stdout line (without its newline) and updates f.
// It reports whether f changed. Implementations are stateless.
type Parser interface {
	Line(line []byte, f *Fields) bool
}

type none struct{}

func (none) Line([]byte, *Fields) bool { return false }

var registry = map[string]Parser{
	"none":               none{},
	"claude-stream-json": claudeStreamJSON{},
	"pi-json":            piJSON{},
	"codex-jsonl":        codexJSONL{},
	"claude-remote":      claudeRemote{},
}

// setsConversation lists the parsers that set Fields.ConversationID.
var setsConversation = map[string]bool{"claude-stream-json": true, "pi-json": true, "codex-jsonl": true}

// SetsConversationID reports whether the named parser sets conversation_id.
func SetsConversationID(name string) bool { return setsConversation[name] }

// Get returns the named parser.
func Get(name string) (Parser, bool) {
	p, ok := registry[name]
	return p, ok
}

// Names lists registered parser names, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
