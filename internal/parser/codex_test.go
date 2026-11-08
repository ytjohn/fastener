package parser

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexJSONLFixture(t *testing.T) {
	p, ok := Get("codex-jsonl")
	if !ok {
		t.Fatal("codex-jsonl not registered")
	}
	got := feed(t, p, "testdata/codex-pong.jsonl")
	if got.ConversationID != "00000000-0000-4000-8000-000000000001" {
		t.Errorf("ConversationID = %q", got.ConversationID)
	}
	if got.FinalMessage != "pong" {
		t.Errorf("FinalMessage = %q", got.FinalMessage)
	}
	if got.Model != "" || got.ModelSource != "" {
		t.Errorf("Model = %q/%q: codex does not report the served model", got.Model, got.ModelSource)
	}
	if got.AgentError != nil && *got.AgentError {
		t.Errorf("AgentError = %v", got.AgentError)
	}
	if got.Usage == nil || got.Usage.InputTokens == nil || *got.Usage.InputTokens != 21164 || got.Usage.OutputTokens == nil || *got.Usage.OutputTokens != 5 || got.Usage.CostUSD != nil {
		t.Errorf("Usage = %+v", got.Usage)
	}
	if b, _ := json.Marshal(got.Usage); string(b) != `{"input_tokens":21164,"output_tokens":5}` {
		t.Errorf("usage JSON = %s", b)
	}
	if got.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d", got.ParseErrors)
	}
}

func TestCodexJSONLModelErrorFixture(t *testing.T) {
	p, _ := Get("codex-jsonl")
	got := feed(t, p, "testdata/codex-model-error.jsonl")
	if got.AgentError == nil || !*got.AgentError {
		t.Errorf("AgentError = %v, want true", got.AgentError)
	}
	if !strings.Contains(got.FinalMessage, "is not supported") || strings.Contains(got.FinalMessage, "metadata") {
		t.Errorf("FinalMessage = %q, want the turn.failed message, not the warning item", got.FinalMessage)
	}
	if got.ConversationID == "" || got.Usage != nil || got.ParseErrors != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestCodexJSONLUsageSummedAcrossTurns(t *testing.T) {
	p, _ := Get("codex-jsonl")
	var f Fields
	p.Line([]byte(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"cache_write_input_tokens":5,"output_tokens":10,"reasoning_output_tokens":4}}`), &f)
	p.Line([]byte(`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":150,"output_tokens":20}}`), &f)
	if f.Usage == nil || f.Usage.InputTokens == nil || *f.Usage.InputTokens != 300 || f.Usage.OutputTokens == nil || *f.Usage.OutputTokens != 30 || f.Usage.CostUSD != nil {
		t.Fatalf("Usage = %+v (cached, cache-write and reasoning tokens must not be added)", f.Usage)
	}
}

func TestCodexJSONLLastAgentMessageWins(t *testing.T) {
	p, _ := Get("codex-jsonl")
	var f Fields
	p.Line([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`), &f)
	p.Line([]byte(`{"type":"item.completed","item":{"type":"command_execution","text":"ignored"}}`), &f)
	p.Line([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"last"}}`), &f)
	p.Line([]byte(`{"type":"item.started","item":{"type":"agent_message","text":"not completed"}}`), &f)
	if f.FinalMessage != "last" {
		t.Errorf("FinalMessage = %q", f.FinalMessage)
	}
}

func TestCodexJSONLErrorItemAloneIsNotAnAgentError(t *testing.T) {
	p, _ := Get("codex-jsonl")
	var f Fields
	changed := p.Line([]byte(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata not found"}}`), &f)
	if changed || f.AgentError != nil || f.FinalMessage != "" {
		t.Errorf("error item recorded something: changed=%v %+v", changed, f)
	}
}

func TestCodexJSONLErrors(t *testing.T) {
	p, _ := Get("codex-jsonl")
	var f Fields
	p.Line([]byte(`{"type":"error","message":"top one"}`), &f)
	p.Line([]byte(`{"type":"error","message":"top two"}`), &f)
	if f.AgentError == nil || !*f.AgentError || f.FinalMessage != "top two" {
		t.Fatalf("last top-level error should win: %+v", f)
	}
	p.Line([]byte(`{"type":"turn.failed","error":{"message":"failed msg"}}`), &f)
	if !*f.AgentError || f.FinalMessage != "failed msg" {
		t.Fatalf("turn.failed after error: %+v", f)
	}
	p.Line([]byte(`{"type":"error","message":"late top"}`), &f)
	if !*f.AgentError || f.FinalMessage != "late top" {
		t.Fatalf("the last error event should win: %+v", f)
	}
	// A turn.failed without a message still flags the error.
	var g Fields
	p.Line([]byte(`{"type":"turn.failed"}`), &g)
	if g.AgentError == nil || !*g.AgentError {
		t.Fatalf("bare turn.failed: %+v", g)
	}
}

func TestCodexJSONLUnknownAndMalformed(t *testing.T) {
	p, _ := Get("codex-jsonl")
	var f Fields
	for _, l := range []string{``, `   `, `{"type":"mystery.event","x":1}`, `{"no":"type"}`, `{"type":"item.completed"}`, `{"type":"item.completed","item":{"type":"future_item"}}`} {
		if p.Line([]byte(l), &f) {
			t.Errorf("line %q changed fields", l)
		}
	}
	if f.ParseErrors != 0 {
		t.Fatalf("unknown input counted as parse error: %+v", f)
	}
	if !p.Line([]byte(`not json`), &f) || !p.Line([]byte(`{"type":"turn.com`), &f) || f.ParseErrors != 2 {
		t.Fatalf("malformed lines: %+v", f)
	}
	// Type mismatch: counted, but the rest of the event is kept.
	p.Line([]byte(`{"type":"turn.completed","usage":{"input_tokens":"x","output_tokens":2}}`), &f)
	p.Line([]byte(`{"type":"thread.started","thread_id":"t1","extra":5}`), &f)
	if f.ParseErrors != 3 || f.ConversationID != "t1" {
		t.Errorf("recovery: %+v", f)
	}
}
