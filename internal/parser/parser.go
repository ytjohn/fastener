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
}

// Usage counts all tokens the backend reported for the run.
type Usage struct {
	// InputTokens is all input tokens for the run, including cache reads and
	// writes (differs from Anthropic's usage.input_tokens, which excludes cache).
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
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
	"pi-json":            piJSON{},
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
