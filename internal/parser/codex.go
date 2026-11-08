package parser

import (
	"bytes"
	"encoding/json"
	"errors"
)

// codexJSONL parses `codex exec --json`. Basis: codex-cli 0.167.0.
//   - thread.started carries thread_id, the conversation id.
//   - item.completed with an agent_message item carries the reply; the last
//     one is the final message.
//   - turn.completed carries usage, summed over every turn in the run.
//   - turn.failed and a top-level error event mean the run failed
//     (agent_error); the message comes from turn.failed's error.message, else
//     the last top-level error's message.
//   - an item.completed with an "error" item is only a warning (for example
//     "Model metadata not found"); the real failure follows as error and
//     turn.failed events, so the item alone records nothing.
//
// Usage: input_tokens is Codex's own reported total. In OpenAI semantics
// cached_input_tokens and reasoning_output_tokens are subsets of input_tokens
// and output_tokens, so they are not added. cache_write_input_tokens is not
// added either: the captures cannot confirm it is excluded from input_tokens.
// Codex reports no cost, so cost_usd stays absent.
//
// Codex does not report the model it served in these events, so Model and
// ModelSource stay empty and `check` skips its model step.
type codexJSONL struct{}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"` // top-level "error"
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"` // turn.failed
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (codexJSONL) Line(line []byte, f *Fields) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	var e codexEvent
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
	switch e.Type {
	case "thread.started":
		if e.ThreadID != "" && e.ThreadID != f.ConversationID {
			f.ConversationID = e.ThreadID
			changed = true
		}
	case "item.completed":
		if e.Item != nil && e.Item.Type == "agent_message" && e.Item.Text != f.FinalMessage {
			f.FinalMessage = e.Item.Text
			changed = true
		}
	case "turn.completed":
		if e.Usage != nil {
			u := Usage{}
			var in, out int64
			if f.Usage != nil {
				u = *f.Usage
				if u.InputTokens != nil {
					in = *u.InputTokens
				}
				if u.OutputTokens != nil {
					out = *u.OutputTokens
				}
			}
			u.InputTokens = i64(in + e.Usage.InputTokens)
			u.OutputTokens = i64(out + e.Usage.OutputTokens)
			f.Usage = &u
			changed = true
		}
	case "turn.failed":
		changed = codexFail(f, e.errorMessage(), true) || changed
	case "error":
		changed = codexFail(f, e.Message, false) || changed
	}
	return changed
}

func (e *codexEvent) errorMessage() string {
	if e.Error != nil {
		return e.Error.Message
	}
	return ""
}

// codexFail marks the run failed. A turn.failed message is authoritative and a
// later top-level error does not replace it; before one arrives, the last
// top-level error's message stands.
func codexFail(f *Fields, msg string, authoritative bool) bool {
	changed := f.AgentError == nil || !*f.AgentError
	t := true
	f.AgentError = &t
	if msg != "" && (authoritative || !f.codexTurnFailed) && msg != f.FinalMessage {
		f.FinalMessage = msg
		changed = true
	}
	if authoritative {
		f.codexTurnFailed = true
	}
	return changed
}
