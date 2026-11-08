package parser

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// piJSON parses `pi -p --mode json`. The "session" event carries the
// conversation id. Each assistant "message_end" carries the provider and
// model, its content blocks, stopReason (and errorMessage on "error" or
// "aborted") and its own usage. Model, final message and error come from the
// last one; usage is summed over all of them. Streaming events (message_update
// and friends) are ignored.
//
// Which model answered depends on pi's API adapter. Basis: pi 0.84.4
// (pi-ai/dist/api/*):
//   - anthropic-messages overwrites message.model with the server's id;
//   - openai-completions keeps the requested id in model and sets
//     responseModel only when the server reported a different one, so both
//     cases are server-reported;
//   - every other adapter only echoes the requested id: openai-responses,
//     azure-openai-responses, openai-codex-responses, bedrock-converse-stream,
//     google-generative-ai, google-vertex, mistral-conversations, pi-messages
//     and cloudflare. These, and any unknown adapter, are client-reported.
//
// After an agent error Model is cleared: the attempted model never answered.
type piJSON struct{}

type piEvent struct {
	Type    string     `json:"type"`
	ID      string     `json:"id"`
	Message *piMessage `json:"message"`
}

type piMessage struct {
	Role          string `json:"role"`
	API           string `json:"api"`
	ResponseModel string `json:"responseModel"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	StopReason    string `json:"stopReason"`
	ErrorMessage  string `json:"errorMessage"`
	Content       []struct {
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
	isErr := m.StopReason == "error" || m.StopReason == "aborted"
	f.AgentError = &isErr
	f.Model, f.ModelSource = "", ""
	served := m.Model
	if m.ResponseModel != "" {
		served = m.ResponseModel
	}
	if served != "" && !isErr {
		f.Model, f.ModelSource = served, "client"
		if m.Provider != "" {
			f.Model = m.Provider + "/" + served
		}
		if m.ResponseModel != "" || m.API == "anthropic-messages" || m.API == "openai-completions" {
			f.ModelSource = "server"
		}
	}
	var text strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	f.FinalMessage = text.String()
	switch {
	case isErr && m.ErrorMessage != "":
		f.FinalMessage = m.ErrorMessage
	case m.StopReason == "aborted":
		f.FinalMessage = "Request aborted" // as pi's print mode reports it
	}
	if m.Usage != nil {
		// Usage is per API call; sum it so multi-turn runs count every call.
		var in, out int64
		u := Usage{}
		if f.Usage != nil {
			u = *f.Usage
			if u.InputTokens != nil {
				in = *u.InputTokens
			}
			if u.OutputTokens != nil {
				out = *u.OutputTokens
			}
		}
		u.InputTokens = i64(in + m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheWrite)
		u.OutputTokens = i64(out + m.Usage.Output)
		if m.Usage.Cost != nil {
			c := m.Usage.Cost.Total
			if u.CostUSD != nil {
				c += *u.CostUSD
			}
			u.CostUSD = &c
		}
		f.Usage = &u
	}
	return f.Model != before.Model || f.ModelSource != before.ModelSource || f.FinalMessage != before.FinalMessage ||
		before.AgentError == nil || *before.AgentError != isErr ||
		(f.Usage != nil && (before.Usage == nil || !f.Usage.equal(*before.Usage)))
}
