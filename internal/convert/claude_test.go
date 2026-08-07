package convert

import (
	"encoding/json"
	"testing"
)

func TestClaudeRequestUnmarshal(t *testing.T) {
	data := []byte(`{"model":"claude-3","max_tokens":1024,"system":"sys","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}],"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}]}`)
	var req MessagesRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 1024 || req.System != "sys" {
		t.Fatal("fields")
	}
	blocks, ok := req.Messages[0].Content.([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("blocks: %#v", req.Messages[0].Content)
	}
}

func TestClaudeRequestUnmarshal_SystemArray(t *testing.T) {
	data := []byte(`{"model":"claude-3","max_tokens":1024,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	var req MessagesRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if got := SystemString(req.System); got != "a\nb" {
		t.Fatalf("system: %q", got)
	}
}

func TestClaudeStreamEventUnmarshal(t *testing.T) {
	data := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	var ev StreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != EventContentBlockDelta || ev.Delta == nil || ev.Delta.Text != "hi" {
		t.Fatalf("event: %+v", ev)
	}
}
