package parser

import (
	"strings"
	"testing"
)

func TestPiJSONFixture(t *testing.T) {
	p, ok := Get("pi-json")
	if !ok {
		t.Fatal("pi-json not registered")
	}
	got := feed(t, p, "testdata/pi-pong.jsonl")
	if got.ConversationID != "00000000-0000-4000-8000-000000000001" {
		t.Errorf("ConversationID = %q", got.ConversationID)
	}
	if got.FinalMessage != "pong" {
		t.Errorf("FinalMessage = %q (thinking must be skipped)", got.FinalMessage)
	}
	if got.Model != "github-copilot/claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q (want the served id from message_end, not message_start)", got.Model)
	}
	if got.ModelSource != "server" {
		t.Errorf("ModelSource = %q (anthropic-messages reports the server's id)", got.ModelSource)
	}
	if got.AgentError == nil || *got.AgentError {
		t.Errorf("AgentError = %v, want false", got.AgentError)
	}
	if got.Usage == nil || got.Usage.InputTokens != 10+0+7880 || got.Usage.OutputTokens != 47 || got.Usage.CostUSD != 0.010095 {
		t.Errorf("Usage = %+v", got.Usage)
	}
	if got.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d", got.ParseErrors)
	}
}

func TestPiJSONModelErrorFixture(t *testing.T) {
	p, _ := Get("pi-json")
	got := feed(t, p, "testdata/pi-model-error.jsonl")
	if got.AgentError == nil || !*got.AgentError {
		t.Errorf("AgentError = %v, want true", got.AgentError)
	}
	if got.FinalMessage == "" || !strings.Contains(got.FinalMessage, "model_not_supported") {
		t.Errorf("FinalMessage = %q, want the errorMessage", got.FinalMessage)
	}
	if got.Model != "" || got.ModelSource != "" {
		t.Errorf("Model = %q, ModelSource = %q: the attempted model never answered, so both must be empty", got.Model, got.ModelSource)
	}
	if got.ConversationID == "" || got.ParseErrors != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestPiJSONLastAssistantWins(t *testing.T) {
	p, _ := Get("pi-json")
	var f Fields
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","provider":"a","model":"m1","content":[{"type":"text","text":"first"}],"stopReason":"toolUse"}}`), &f)
	p.Line([]byte(`{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"tool output"}]}}`), &f)
	p.Line([]byte(`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"user text"}]}}`), &f)
	if f.FinalMessage != "first" || f.Model != "a/m1" {
		t.Fatalf("non-assistant message_end changed fields: %+v", f)
	}
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","model":"m2","content":[{"type":"text","text":"a"},{"type":"thinking","thinking":"x"},{"type":"text","text":"b"}],"stopReason":"stop"}}`), &f)
	if f.FinalMessage != "ab" || f.Model != "m2" || f.AgentError == nil || *f.AgentError {
		t.Fatalf("last assistant message not taken: %+v", f)
	}
	// A last message without a model clears it rather than keeping a stale one.
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"c"}],"stopReason":"stop"}}`), &f)
	if f.Model != "" || f.ModelSource != "" {
		t.Fatalf("stale model kept: %+v", f)
	}
	if f.Usage != nil {
		t.Errorf("usage fabricated: %+v", f.Usage)
	}
}

func TestPiJSONMalformedUnknownAndMismatch(t *testing.T) {
	p, _ := Get("pi-json")
	var f Fields
	if p.Line([]byte(""), &f) {
		t.Error("blank line reported a change")
	}
	if !p.Line([]byte("{not json"), &f) || f.ParseErrors != 1 {
		t.Errorf("malformed: ParseErrors = %d", f.ParseErrors)
	}
	if p.Line([]byte(`{"type":"mystery","id":"zzz"}`), &f) || f.ConversationID != "" {
		t.Errorf("unknown type changed fields: %+v", f)
	}
	if !p.Line([]byte(`{"type":"session","id":"s1"}`), &f) || f.ConversationID != "s1" {
		t.Errorf("session id: %+v", f)
	}
	if p.Line([]byte(`{"type":"session","id":"s1"}`), &f) {
		t.Error("repeated session id reported a change")
	}
	// usage.input is a string: the rest of the event is still taken.
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","provider":"p","model":"m","content":[{"type":"text","text":"hi"}],"stopReason":"stop","usage":{"input":"x","output":2}}}`), &f)
	if f.ParseErrors != 2 || f.FinalMessage != "hi" || f.Model != "p/m" {
		t.Errorf("type mismatch lost the event: %+v", f)
	}
}

func TestPiJSONUsageSummedAcrossRun(t *testing.T) {
	p, _ := Get("pi-json")
	var f Fields
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","provider":"p","model":"m1","content":[{"type":"text","text":"calling a tool"}],"stopReason":"toolUse","usage":{"input":10,"output":5,"cacheRead":100,"cacheWrite":1000,"cost":{"total":0.25}}}}`), &f)
	p.Line([]byte(`{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"ok"}],"usage":{"input":999,"output":999}}}`), &f)
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","provider":"p","model":"m2","content":[{"type":"text","text":"done"}],"stopReason":"stop","usage":{"input":20,"output":7,"cacheRead":1110,"cacheWrite":0,"cost":{"total":0.5}}}}`), &f)
	if f.Usage == nil || f.Usage.InputTokens != 10+100+1000+20+1110 || f.Usage.OutputTokens != 12 || f.Usage.CostUSD != 0.75 {
		t.Fatalf("Usage = %+v", f.Usage)
	}
	if f.Model != "p/m2" || f.FinalMessage != "done" || f.AgentError == nil || *f.AgentError {
		t.Fatalf("last message fields: %+v", f)
	}
}

func TestPiJSONResponseModelAndSource(t *testing.T) {
	p, _ := Get("pi-json")
	cases := []struct{ line, model, source string }{
		// openai-completions: responseModel is set only when the server served something else.
		{`{"type":"message_end","message":{"role":"assistant","api":"openai-completions","provider":"p","model":"qwen3-x","responseModel":"glm-5","content":[{"type":"text","text":"pong"}],"stopReason":"stop"}}`, "p/glm-5", "server"},
		{`{"type":"message_end","message":{"role":"assistant","api":"openai-completions","provider":"p","model":"qwen3-x","content":[{"type":"text","text":"pong"}],"stopReason":"stop"}}`, "p/qwen3-x", "server"},
		// openai-responses records only the requested id.
		{`{"type":"message_end","message":{"role":"assistant","api":"openai-responses","provider":"p","model":"gpt-5","content":[{"type":"text","text":"pong"}],"stopReason":"stop"}}`, "p/gpt-5", "client"},
		{`{"type":"message_end","message":{"role":"assistant","api":"anthropic-messages","model":"claude-haiku-4-5","content":[{"type":"text","text":"pong"}],"stopReason":"stop"}}`, "claude-haiku-4-5", "server"},
	}
	for _, c := range cases {
		var f Fields
		p.Line([]byte(c.line), &f)
		if f.Model != c.model || f.ModelSource != c.source {
			t.Errorf("%s\n  got model %q source %q, want %q %q", c.line, f.Model, f.ModelSource, c.model, c.source)
		}
	}
}

func TestPiJSONAborted(t *testing.T) {
	p, _ := Get("pi-json")
	var f Fields
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","provider":"p","model":"m","content":[{"type":"text","text":"partial"}],"stopReason":"aborted"}}`), &f)
	if f.AgentError == nil || !*f.AgentError || f.FinalMessage != "Request aborted" || f.Model != "" {
		t.Fatalf("aborted without errorMessage: %+v", f)
	}
	p.Line([]byte(`{"type":"message_end","message":{"role":"assistant","provider":"p","model":"m","content":[],"stopReason":"aborted","errorMessage":"cancelled by user"}}`), &f)
	if f.AgentError == nil || !*f.AgentError || f.FinalMessage != "cancelled by user" {
		t.Fatalf("aborted with errorMessage: %+v", f)
	}
}
