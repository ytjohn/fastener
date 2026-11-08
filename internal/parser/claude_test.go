package parser

import (
	"bufio"
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
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p.Line(sc.Bytes(), &out)
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
