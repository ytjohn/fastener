package parser

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// piJSON parses `pi -p --mode json`. The "session" event carries the
// conversation id. Each assistant "message_end" carries the served provider
// and model, its content blocks, stopReason (and errorMessage on "error") and
// its own usage. Model, final message and error come from the last one; usage
// is summed over all of them. Streaming events (message_update and friends) are
// ignored.
type piJSON struct{}

type piEvent struct {
	Type    string     `json:"type"`
	ID      string     `json:"id"`
	Message *piMessage `json:"message"`
}

type piMessage struct {
	Role         string `json:"role"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
	Content      []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage *struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cacheRead"`
		CacheWrite int64 `json:"cacheWrite"`
		Cost       *struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

func (piJSON) Line(line []byte, f *Fields) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	var e piEvent
	changed := false
	if err := json.Unmarshal(line, &e); err != nil {
		f.ParseErrors++
		changed = true
		// A type mismatch leaves the rest of the event decoded; keep it.
		var ute *json.UnmarshalTypeError
		if !errors.As(err, &ute) {
			return true
		}
	}
	switch {
	case e.Type == "session":
		if e.ID != "" && e.ID != f.ConversationID {
			f.ConversationID = e.ID
			changed = true
		}
	case e.Type == "message_end" && e.Message != nil && e.Message.Role == "assistant":
		if piAssistant(e.Message, f) {
			changed = true
		}
	}
	return changed
}

func piAssistant(m *piMessage, f *Fields) bool {
	before := *f
	if m.Model != "" {
		f.Model = m.Model
		if m.Provider != "" {
			f.Model = m.Provider + "/" + m.Model
		}
	}
	isErr := m.StopReason == "error"
	f.AgentError = &isErr
	var text strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	f.FinalMessage = text.String()
	if isErr && m.ErrorMessage != "" {
		f.FinalMessage = m.ErrorMessage
	}
	if m.Usage != nil {
		// Usage is per API call; sum it so multi-turn runs count every call.
		u := Usage{}
		if f.Usage != nil {
			u = *f.Usage
		}
		u.InputTokens += m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheWrite
		u.OutputTokens += m.Usage.Output
		if m.Usage.Cost != nil {
			u.CostUSD += m.Usage.Cost.Total
		}
		f.Usage = &u
	}
	return f.Model != before.Model || f.FinalMessage != before.FinalMessage ||
		before.AgentError == nil || *before.AgentError != isErr ||
		(f.Usage != nil && (before.Usage == nil || *f.Usage != *before.Usage))
}
