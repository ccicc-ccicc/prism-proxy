package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeEvent 从一帧 SSE 中解出 data 行的 StreamEvent 负载。
func decodeEvent(t *testing.T, f []byte) StreamEvent {
	t.Helper()
	s := string(f)
	i := strings.Index(s, "data: ")
	if i < 0 {
		t.Fatalf("no data line in frame: %q", f)
	}
	var ev StreamEvent
	if err := json.Unmarshal([]byte(s[i+len("data: "):len(s)-2]), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestO2CStream_TextFlow(t *testing.T) {
	s := NewO2CStreamWithModel("gpt-4o")
	// 首块：role → message_start
	out, err := s.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	if err != nil || len(out) != 1 {
		t.Fatalf("start: %d %v", len(out), err)
	}
	if !strings.HasPrefix(string(out[0]), "event: message_start") {
		t.Fatalf("first event: %s", out[0])
	}
	// 首个内容 delta → content_block_start(text, 0) + content_block_delta
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	if len(out) != 2 {
		t.Fatalf("content write frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_start") || !strings.Contains(string(out[0]), "\"type\":\"text\"") {
		t.Fatalf("text block start: %s", out[0])
	}
	if !strings.Contains(string(out[1]), "content_block_delta") || !strings.Contains(string(out[1]), "\"type\":\"text_delta\"") || !strings.Contains(string(out[1]), "\"text\":\"hi\"") {
		t.Fatalf("text delta: %s", out[1])
	}
	// finish → content_block_stop(0) 随后 message_delta{end_turn}
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	if len(out) != 2 {
		t.Fatalf("finish write frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("text block stop: %s", out[0])
	}
	if !strings.Contains(string(out[1]), "message_delta") || !strings.Contains(string(out[1]), "\"stop_reason\":\"end_turn\"") {
		t.Fatalf("finish: %s", out[1])
	}
	// Finish() → message_stop（text 块已在 finish chunk 关闭）
	fin := s.Finish()
	if len(fin) != 1 || !strings.Contains(string(fin[0]), "message_stop") {
		t.Fatalf("finish frame: %s", fin[0])
	}
}

func TestO2CStream_ToolCalls(t *testing.T) {
	s := NewO2CStream()
	// 首块即 tool_calls：message_start 仍须先发，随后 content_block_start(tool_use)
	out, err := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	if err != nil || len(out) != 2 {
		t.Fatalf("tool start: %v", err)
	}
	if !strings.HasPrefix(string(out[0]), "event: message_start") {
		t.Fatalf("tool stream must start with message_start: %s", out[0])
	}
	if !strings.Contains(string(out[1]), "content_block_start") || !strings.Contains(string(out[1]), "\"type\":\"tool_use\"") {
		t.Fatalf("tool block start: %s", out[1])
	}
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]},"finish_reason":null}]}`))
	if len(out) != 1 || !strings.Contains(string(out[0]), "input_json_delta") {
		t.Fatalf("args delta: %s", out[0])
	}
	// tool_calls finish → 先关 tool 块 content_block_stop，随后 message_delta{stop_reason=tool_use}
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	if len(out) != 2 {
		t.Fatalf("tool finish frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("tool block stop: %s", out[0])
	}
	if !strings.Contains(string(out[1]), "message_delta") || !strings.Contains(string(out[1]), "\"stop_reason\":\"tool_use\"") {
		t.Fatalf("tool finish: %s", out[1])
	}
}

func TestO2CStream_BlockIndices(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	// 首个内容 delta：text 块 content_block_start 使用全局 index 0
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"t"},"finish_reason":null}]}`))
	if len(out) != 2 {
		t.Fatalf("text write frames: %d", len(out))
	}
	textStart := decodeEvent(t, out[0])
	if textStart.ContentBlock == nil || textStart.ContentBlock.Type != "text" || textStart.Index != 0 {
		t.Fatalf("text block start must be index 0: %+v", textStart)
	}
	// text 块先收 content_block_stop，随后 tool_use 使用全局 index 1
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	if len(out) != 2 || !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("text stop before tool start: %s", out)
	}
	toolStart := decodeEvent(t, out[1])
	if toolStart.ContentBlock == nil || toolStart.ContentBlock.Type != "tool_use" || toolStart.Index != 1 {
		t.Fatalf("tool_use index must be 1: %+v", toolStart)
	}
}

func TestO2CStream_FramesWellFormed(t *testing.T) {
	s := NewO2CStream()
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	f := string(out[0])
	if !strings.HasPrefix(f, "event: message_start\n") || !strings.HasSuffix(f, "\n\n") {
		t.Fatalf("frame format: %q", f)
	}
	var ev StreamEvent
	line := f[strings.Index(f, "data: ")+6 : len(f)-2]
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Message == nil || ev.Message.Role != "assistant" {
		t.Fatalf("message_start payload: %+v", ev.Message)
	}
}

// Critical 1: finish_reason 到达时打开的 tool_use 块必须先 content_block_stop，
// 再 message_delta（块 stop 必须 precede message_delta）。
func TestO2CStream_ToolFinishClosesBlock(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]},"finish_reason":null}]}`))
	// [content_block_stop(idx), message_delta{stop_reason:tool_use}]，顺序固定
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	if len(out) != 2 {
		t.Fatalf("finish frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("tool block must close before message_delta: %s", out[0])
	}
	if !strings.Contains(string(out[1]), "message_delta") || !strings.Contains(string(out[1]), "\"stop_reason\":\"tool_use\"") {
		t.Fatalf("finish: %s", out[1])
	}
}

// Important 2: 纯 tool 流（首块即 tool_calls）第一块 index 必须为 0（无空洞）。
func TestO2CStream_ToolOnlyIndexZero(t *testing.T) {
	s := NewO2CStream()
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	if len(out) != 2 {
		t.Fatalf("tool-only start frames: %d (%s)", len(out), out)
	}
	start := decodeEvent(t, out[1])
	if start.ContentBlock == nil || start.ContentBlock.Type != "tool_use" || start.Index != 0 {
		t.Fatalf("first tool block must be index 0: %+v", start)
	}
}

// Important 3: 单个 chunk 内多个 tool_calls 开始——块间必须 stop 并连续递增
// index（0、1…）；start chunk 自带的 arguments（Minor 7）须在 start 后发射。
func TestO2CStream_MultiToolCallsSameChunk(t *testing.T) {
	s := NewO2CStream()
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}},{"index":1,"id":"c2","type":"function","function":{"name":"f2","arguments":""}}]},"finish_reason":null}]}`))
	// [message_start, start(0), input_json_delta(0), stop(0), start(1)]
	if len(out) != 5 {
		t.Fatalf("multi tool frames: %d (%s)", len(out), out)
	}
	start1 := decodeEvent(t, out[1])
	if start1.ContentBlock == nil || start1.ContentBlock.Type != "tool_use" || start1.Index != 0 {
		t.Fatalf("first tool start: %+v", start1)
	}
	argsDelta := decodeEvent(t, out[2])
	if argsDelta.Delta == nil || argsDelta.Delta.Type != "input_json_delta" || argsDelta.Delta.PartialJSON != `{"a":1}` {
		t.Fatalf("args in start chunk: %+v", argsDelta.Delta)
	}
	if !strings.Contains(string(out[3]), "content_block_stop") {
		t.Fatalf("stop between tool blocks: %s", out[3])
	}
	start2 := decodeEvent(t, out[4])
	if start2.ContentBlock == nil || start2.ContentBlock.Type != "tool_use" || start2.Index != 1 {
		t.Fatalf("second tool start: %+v", start2)
	}
}

// Minor 7: tool_calls start chunk 自带非空 arguments → start 后补 input_json_delta。
func TestO2CStream_ToolStartWithArguments(t *testing.T) {
	s := NewO2CStream()
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]},"finish_reason":null}]}`))
	// [message_start, start(0), input_json_delta(0)]
	if len(out) != 3 {
		t.Fatalf("frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[1]), "content_block_start") {
		t.Fatalf("start: %s", out[1])
	}
	argsDelta := decodeEvent(t, out[2])
	if argsDelta.Delta == nil || argsDelta.Delta.Type != "input_json_delta" || argsDelta.Delta.PartialJSON != `{"x":1}` {
		t.Fatalf("args after start: %+v", argsDelta.Delta)
	}
}

// Minor 5: usage chunk → message_delta{usage}，prompt/completion tokens 正确映射。
func TestO2CStream_UsageDelta(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	if len(out) != 1 || !strings.Contains(string(out[0]), "message_delta") {
		t.Fatalf("usage frame: %s", out[0])
	}
	ev := decodeEvent(t, out[0])
	if ev.Usage == nil || ev.Usage.InputTokens != 10 || ev.Usage.OutputTokens != 5 {
		t.Fatalf("usage payload: %+v", ev.Usage)
	}
}

// Minor 5: finish_reason=length → stop_reason=max_tokens。
func TestO2CStream_LengthStopReason(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`))
	if !strings.Contains(string(out[0]), "\"stop_reason\":\"max_tokens\"") {
		t.Fatalf("length finish: %s", out[0])
	}
}

// Minor 6: Finish() 幂等——第二次调用不再产出帧。
func TestO2CStream_FinishIdempotent(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	first := s.Finish()
	if len(first) != 1 || !strings.Contains(string(first[0]), "message_stop") {
		t.Fatalf("first Finish: %s", first)
	}
	if second := s.Finish(); len(second) != 0 {
		t.Fatalf("second Finish must emit nothing, got %d frames", len(second))
	}
}

// Important (round 2): tool_use 块打开时到达 content delta → 统一切换：
// [content_block_stop(0), content_block_start(text,1), content_block_delta]，
// tool→text 的 index 必须 0、1 连续（内容不得被静默丢弃）。
func TestO2CStream_ToolThenText(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	// [content_block_stop(0), content_block_start(text,1), content_block_delta]
	if len(out) != 3 {
		t.Fatalf("tool-then-text frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("tool block must close: %s", out[0])
	}
	textStart := decodeEvent(t, out[1])
	if textStart.ContentBlock == nil || textStart.ContentBlock.Type != "text" || textStart.Index != 1 {
		t.Fatalf("text block must be index 1: %+v", textStart)
	}
	delta := decodeEvent(t, out[2])
	if delta.Delta == nil || delta.Delta.Type != "text_delta" || delta.Delta.Text != "hi" {
		t.Fatalf("text delta: %+v", delta.Delta)
	}
}

// 连续 content delta 属于同一 text 块（简化约定：不逐 chunk 拆块）：
// 第二个 content chunk 只发射 content_block_delta，无 stop、index 不变。
func TestO2CStream_TextContinuationNoSplit(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"there"},"finish_reason":null}]}`))
	if len(out) != 1 {
		t.Fatalf("continuation frames: %d (%s)", len(out), out)
	}
	ev := decodeEvent(t, out[0])
	if ev.Index != 0 || ev.Delta == nil || ev.Delta.Type != "text_delta" || ev.Delta.Text != "there" {
		t.Fatalf("continuation delta: %+v", ev)
	}
}
