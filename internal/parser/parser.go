// Package parser extracts optional, mechanical facts from an agent's stdout,
// one line at a time. Parsers never affect the session: a bad line only
// increments ParseErrors.
package parser

import "sort"

// Fields are written into status.json; every field is optional.
type Fields struct {
	ConversationID string `json:"conversation_id,omitempty"`
	FinalMessage   string `json:"final_message,omitempty"`
	AgentError     *bool  `json:"agent_error,omitempty"`
	Usage          *Usage `json:"usage,omitempty"`
	ParseErrors    int    `json:"parse_errors,omitempty"`
}

// Usage counts all tokens the backend reported for the run.
type Usage struct {
	InputTokens  int64   `json:"input_tokens,omitempty"` // includes cache reads/writes
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

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
}

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
