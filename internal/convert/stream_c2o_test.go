package convert

import (
	"bytes"
	"encoding/json"
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

// Critical 1: 上游 error 事件 → Write 返回 error（绝不静默吞掉并伪造成功），
// 错误信息取自 payload 的 error.message。
func TestC2OStream_ErrorEvent(t *testing.T) {
	s := NewC2OStream("claude-3")
	out, err := s.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n"))
	if err != nil || len(out) != 1 {
		t.Fatalf("start: %v %s", err, out)
	}
	_, err = s.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	if err == nil || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("error event must fail the stream, got %v", err)
	}
	// 无 message 的 error 事件 → 同样报错（通用消息）
	_, err = s.Write([]byte("event: error\ndata: {\"type\":\"error\"}\n\n"))
	if err == nil {
		t.Fatal("bare error event must fail the stream")
	}
}

func TestParseClaudeFrame(t *testing.T) {
	event, data, err := ParseClaudeFrame([]byte("event: content_block_delta\ndata: {\"x\":1}\n\n"))
	if err != nil || event != EventContentBlockDelta || string(data) != `{"x":1}` {
		t.Fatalf("parse: %s %s %v", event, data, err)
	}
}

// 连续两个 tool_use 块（各 start → delta → stop）：toolIdx 独立计数，
// 第一块 tool_calls index=0、第二块 index=1，参数增量携带当前块的 index。
func TestC2OStream_ToolCalls_MultipleBlocks(t *testing.T) {
	s := NewC2OStream("claude-3")
	var out [][]byte
	for _, f := range []string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f1\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":1}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_2\",\"name\":\"f2\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"y\\\":2}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
	} {
		frames, err := s.Write([]byte(f))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, frames...)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(out))
	}
	// 两块的 start 与 args 增量：tool_calls index 依次为 0、0、1、1
	want := []int{0, 0, 1, 1}
	for i, w := range want {
		line := bytes.TrimSuffix(bytes.TrimPrefix(out[i], []byte("data: ")), []byte("\n\n"))
		var ch StreamChunk
		if err := json.Unmarshal(line, &ch); err != nil {
			t.Fatalf("chunk %d unmarshal: %v", i, err)
		}
		if len(ch.Choices) != 1 || len(ch.Choices[0].Delta.ToolCalls) != 1 || ch.Choices[0].Delta.ToolCalls[0].Index == nil {
			t.Fatalf("chunk %d: unexpected tool_calls shape: %s", i, line)
		}
		if got := *ch.Choices[0].Delta.ToolCalls[0].Index; got != w {
			t.Fatalf("chunk %d: tool_calls index = %d, want %d", i, got, w)
		}
	}
	// 顺序校验：第一块 id=toolu_1、第二块 id=toolu_2
	if !strings.Contains(string(out[0]), "\"id\":\"toolu_1\"") || !strings.Contains(string(out[2]), "\"id\":\"toolu_2\"") {
		t.Fatalf("tool ids out of order: %s / %s", out[0], out[2])
	}
}
