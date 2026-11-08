package parser

import (
	"bytes"
	"encoding/json"
	"errors"
)

// claudeStreamJSON parses `claude -p --output-format stream-json --verbose`.
// Every event carries session_id; the final "result" event carries the reply,
// is_error, total_cost_usd and usage. The system/init event names the model.
type claudeStreamJSON struct{}

type claudeEvent struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	Model        string   `json:"model"`
	SessionID    string   `json:"session_id"`
	Result       *string  `json:"result"`
	IsError      *bool    `json:"is_error"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (claudeStreamJSON) Line(line []byte, f *Fields) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	var e claudeEvent
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
	if e.SessionID != "" && e.SessionID != f.ConversationID {
		f.ConversationID = e.SessionID
		changed = true
	}
	if e.Type == "system" && e.Subtype == "init" && e.Model != "" && e.Model != f.Model {
		f.Model = e.Model
		changed = true
	}
	if e.Type != "result" {
		return changed
	}
	if e.Result != nil {
		f.FinalMessage = *e.Result
		changed = true
	}
	if e.IsError != nil {
		v := *e.IsError
		f.AgentError = &v
		changed = true
	}
	if e.Usage != nil {
		u := &Usage{
			InputTokens:  e.Usage.InputTokens + e.Usage.CacheCreationInputTokens + e.Usage.CacheReadInputTokens,
			OutputTokens: e.Usage.OutputTokens,
		}
		if f.Usage != nil {
			u.CostUSD = f.Usage.CostUSD
		}
		f.Usage = u
		changed = true
	}
	if e.TotalCostUSD != nil && f.Usage != nil {
		f.Usage.CostUSD = *e.TotalCostUSD
		changed = true
	}
	return changed
}
