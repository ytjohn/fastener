package parser

import (
	"bytes"
	"encoding/json"
	"errors"
)

// claudeStreamJSON parses `claude -p --output-format stream-json --verbose`.
// Every event carries session_id; the final "result" event carries the reply,
// is_error, total_cost_usd and usage.
//
// The served model is the last top-level assistant event's message.model
// (subagent turns, with a parent_tool_use_id, are skipped), which the
// Anthropic API reports. Until one arrives, the system/init event's model (the
// CLI's own setting) stands in as client-reported. An error result clears it.
type claudeStreamJSON struct{}

type claudeEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message *struct {
		Model string `json:"model"`
	} `json:"message"`
	// ParentToolUseID is set on a subagent's turns, which may run on another model.
	ParentToolUseID *string  `json:"parent_tool_use_id"`
	SessionID       string   `json:"session_id"`
	Result          *string  `json:"result"`
	IsError         *bool    `json:"is_error"`
	TotalCostUSD    *float64 `json:"total_cost_usd"`
	Usage           *struct {
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
	switch {
	case e.Type == "system" && e.Subtype == "init" && e.Model != "" && f.ModelSource != "server":
		changed = setModel(f, e.Model, "client") || changed
	case e.Type == "assistant" && e.ParentToolUseID == nil && e.Message != nil && e.Message.Model != "" && e.Message.Model != "<synthetic>":
		changed = setModel(f, e.Message.Model, "server") || changed
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
		if v {
			f.Model, f.ModelSource = "", ""
		}
	}
	if e.Usage != nil {
		u := &Usage{
			InputTokens:  i64(e.Usage.InputTokens + e.Usage.CacheCreationInputTokens + e.Usage.CacheReadInputTokens),
			OutputTokens: i64(e.Usage.OutputTokens),
		}
		if f.Usage != nil {
			if f.Usage.CostUSD != nil {
				c := *f.Usage.CostUSD
				u.CostUSD = &c
			}
		}
		f.Usage = u
		changed = true
	}
	if e.TotalCostUSD != nil {
		if f.Usage == nil {
			f.Usage = &Usage{} // cost only: keep it without inventing token counts
		}
		c := *e.TotalCostUSD
		f.Usage.CostUSD = &c
		changed = true
	}
	return changed
}

func setModel(f *Fields, model, source string) bool {
	if f.Model == model && f.ModelSource == source {
		return false
	}
	f.Model, f.ModelSource = model, source
	return true
}
