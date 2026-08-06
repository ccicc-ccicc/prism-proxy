package convert

import (
	"strings"
	"testing"
)

func TestC2OStream_TextFlow(t *testing.T) {
	s := NewC2OStream("claude-3")
	out, _ := s.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"role\":\"assistant\"") {
		t.Fatalf("start: %s", out[0])
	}
	out, _ = s.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	if len(out) != 0 {
		t.Fatalf("text block start should be silent: %s", out)
	}
	out, _ = s.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"content\":\"hi\"") {
		t.Fatalf("text delta: %s", out[0])
	}
	out, _ = s.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\n"))
	joined := string(out[len(out)-1])
	if !strings.Contains(joined, "\"finish_reason\":\"stop\"") || !strings.Contains(joined, "\"usage\"") {
		t.Fatalf("finish: %s", joined)
	}
	if got := string(s.Close()[0]); !strings.Contains(got, "[DONE]") {
		t.Fatalf("done: %s", got)
	}
}

func TestC2OStream_ToolCalls_IndependentIndex(t *testing.T) {
	s := NewC2OStream("claude-3")
	// 先 text block（index 0）
	_, _ = s.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	_, _ = s.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"thinking\"}}\n\n"))
	// tool_use block（Claude index 1，OpenAI tool_calls index 必须从 0）
	out, _ := s.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"index\":0") || !strings.Contains(string(out[0]), "\"id\":\"toolu_1\"") {
		t.Fatalf("tool start: %s", out[0])
	}
	out, _ = s.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":1}\"}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"arguments\":\"{\\\"x\\\":1}\"") {
		t.Fatalf("tool delta: %s", out[0])
	}
	// tool finish
	out, _ = s.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"))
	if !strings.Contains(string(out[0]), "\"finish_reason\":\"tool_calls\"") {
		t.Fatalf("tool finish: %s", out[0])
	}
}

func TestC2OStream_ThinkingStripped(t *testing.T) {
	s := NewC2OStream("claude-3")
	outs := [][]byte(nil)
	for _, f := range []string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"secret\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"more\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	} {
		out, err := s.Write([]byte(f))
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, out...)
	}
	if len(outs) != 0 {
		t.Fatalf("thinking events leaked: %d", len(outs))
	}
}

func TestC2OStream_PingAndStatsSwallowed(t *testing.T) {
	s := NewC2OStream("claude-3")
	for _, f := range []string{
		"event: ping\ndata: {}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n",
	} {
		out, err := s.Write([]byte(f))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 0 {
			t.Fatalf("ping/usage-only delta should be swallowed: %d", len(out))
		}
	}
}

func TestParseClaudeFrame(t *testing.T) {
	event, data, err := ParseClaudeFrame([]byte("event: content_block_delta\ndata: {\"x\":1}\n\n"))
	if err != nil || event != EventContentBlockDelta || string(data) != `{"x":1}` {
		t.Fatalf("parse: %s %s %v", event, data, err)
	}
}
