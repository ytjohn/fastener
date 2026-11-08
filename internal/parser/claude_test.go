package parser

import (
	"bufio"
	"bytes"
	"os"
	"testing"
)

func feed(t *testing.T, p Parser, path string) Fields {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out Fields
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			p.Line(bytes.TrimRight(line, "\r\n"), &out)
		}
		if err != nil {
			break
		}
	}
	return out
}

func TestClaudeStreamJSONFixture(t *testing.T) {
	p, ok := Get("claude-stream-json")
	if !ok {
		t.Fatal("claude-stream-json not registered")
	}
	got := feed(t, p, "testdata/claude-pong.jsonl")
	if got.ConversationID != "07a30733-a1d1-48ef-b822-91ae107e12a5" {
		t.Errorf("ConversationID = %q", got.ConversationID)
	}
	if got.FinalMessage != "pong" {
		t.Errorf("FinalMessage = %q", got.FinalMessage)
	}
	if got.Model != "claude-opus-5-5" || got.ModelSource != "server" {
		t.Errorf("Model = %q, ModelSource = %q", got.Model, got.ModelSource)
	}
	if got.AgentError == nil || *got.AgentError {
		t.Errorf("AgentError = %v, want false", got.AgentError)
	}
	if got.Usage == nil || got.Usage.InputTokens != 2+13240+11267 || got.Usage.OutputTokens != 4 || got.Usage.CostUSD != 0.1082614 {
		t.Errorf("Usage = %+v", got.Usage)
	}
	if got.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d", got.ParseErrors)
	}
}

func TestClaudeStreamJSONMalformedAndBlank(t *testing.T) {
	p, _ := Get("claude-stream-json")
	var f Fields
	if p.Line([]byte(""), &f) {
		t.Error("blank line reported a change")
	}
	if !p.Line([]byte("{not json"), &f) || f.ParseErrors != 1 {
		t.Errorf("malformed line: ParseErrors = %d", f.ParseErrors)
	}
	p.Line([]byte(`{"type":"result","is_error":true,"result":"boom","session_id":"s"}`), &f)
	if f.AgentError == nil || !*f.AgentError || f.FinalMessage != "boom" {
		t.Errorf("error result not captured: %+v", f)
	}
}

func TestNoneExtractsNothing(t *testing.T) {
	p, ok := Get("none")
	if !ok {
		t.Fatal("none not registered")
	}
	got := feed(t, p, "testdata/claude-pong.jsonl")
	if got.ConversationID != "" || got.FinalMessage != "" {
		t.Errorf("none extracted %+v", got)
	}
}

func TestUnknownParser(t *testing.T) {
	if _, ok := Get("nope"); ok {
		t.Error("unknown parser reported as registered")
	}
}

func TestClaudeRepeatedSessionAndUnknownType(t *testing.T) {
	p, _ := Get("claude-stream-json")
	var f Fields
	if !p.Line([]byte(`{"type":"system","session_id":"s"}`), &f) {
		t.Error("first session_id should report a change")
	}
	if p.Line([]byte(`{"type":"system","session_id":"s"}`), &f) {
		t.Error("repeated session_id reported a change")
	}
	if p.Line([]byte(`{"type":"mystery"}`), &f) {
		t.Error("unknown-type line reported a change")
	}
}

func TestClaudeTypeMismatchKeepsPartial(t *testing.T) {
	p, _ := Get("claude-stream-json")
	var f Fields
	p.Line([]byte(`{"type":"result","session_id":"s","result":{"x":1}}`), &f)
	if f.ParseErrors != 1 || f.ConversationID != "s" {
		t.Errorf("got %+v", f)
	}
	p.Line([]byte(`{"type":"result","session_id":"s2","is_error":true,"result":{"x":1}}`), &f)
	if f.AgentError == nil || !*f.AgentError || f.ConversationID != "s2" {
		t.Errorf("is_error not captured: %+v", f)
	}
}

func TestClaudeCostOnlyNoFakeTokens(t *testing.T) {
	p, _ := Get("claude-stream-json")
	var f Fields
	p.Line([]byte(`{"type":"result","total_cost_usd":0.5}`), &f)
	if f.Usage != nil {
		t.Fatalf("cost-only result fabricated Usage: %+v", f.Usage)
	}
	p.Line([]byte(`{"type":"result","usage":{"input_tokens":3,"output_tokens":4}}`), &f)
	p.Line([]byte(`{"type":"result","total_cost_usd":0.5}`), &f)
	if f.Usage == nil || f.Usage.InputTokens != 3 || f.Usage.OutputTokens != 4 || f.Usage.CostUSD != 0.5 {
		t.Fatalf("Usage = %+v", f.Usage)
	}
}

func TestClaudeModelPrefersAssistant(t *testing.T) {
	p, _ := Get("claude-stream-json")
	var f Fields
	if p.Line([]byte(`{"type":"result","model":"not-this","session_id":"s"}`), &f); f.Model != "" {
		t.Errorf("result event set Model = %q", f.Model)
	}
	// init alone: the CLI's own setting, so client-reported.
	if !p.Line([]byte(`{"type":"system","subtype":"init","model":"A","session_id":"s"}`), &f) || f.Model != "A" || f.ModelSource != "client" {
		t.Fatalf("init fallback: %+v", f)
	}
	if !p.Line([]byte(`{"type":"assistant","message":{"role":"assistant","model":"B","content":[]},"session_id":"s"}`), &f) || f.Model != "B" || f.ModelSource != "server" {
		t.Fatalf("assistant model not preferred: %+v", f)
	}
	if p.Line([]byte(`{"type":"system","subtype":"init","model":"A","session_id":"s"}`), &f) || f.Model != "B" {
		t.Errorf("a later init overrode the assistant model: %+v", f)
	}
	// Claude Code's synthetic messages (local errors) name no real model.
	p.Line([]byte(`{"type":"assistant","message":{"role":"assistant","model":"<synthetic>","content":[]},"session_id":"s"}`), &f)
	if f.Model != "B" {
		t.Errorf("synthetic model taken: %+v", f)
	}
	p.Line([]byte(`{"type":"assistant","message":{"role":"assistant","model":"C","content":[]},"session_id":"s"}`), &f)
	if f.Model != "C" {
		t.Errorf("last assistant model not taken: %+v", f)
	}
	// An error result clears it: the model is not proven to have answered.
	p.Line([]byte(`{"type":"result","is_error":true,"result":"API Error","session_id":"s"}`), &f)
	if f.Model != "" || f.ModelSource != "" {
		t.Errorf("model kept after an error result: %+v", f)
	}
}
