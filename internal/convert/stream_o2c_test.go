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
	// Important 2: data 负载的 type 必须等于事件名（真实 Anthropic 线格式）
	if ev.Type != EventMessageStart {
		t.Fatalf("data.type = %q, want %q (%s)", ev.Type, EventMessageStart, f)
	}
}

// Important 2: 每帧 data 负载的 type 字段必须等于事件名（message_start、message_stop）。
func TestO2CStream_FrameDataTypeMatchesEvent(t *testing.T) {
	s := NewO2CStream()
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out = append(out, s.Finish()...)
	for i, want := range []string{EventMessageStart, EventMessageStop} {
		ev := decodeEvent(t, out[i])
		if ev.Type != want {
			t.Fatalf("frame %d: data.type = %q, want %q (%s)", i, ev.Type, want, out[i])
		}
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

// Important 3: message_start 的 message 必须携带 stop_reason:null、stop_sequence:null 与 usage。
func TestO2CStream_MessageStartSchema(t *testing.T) {
	s := NewO2CStreamWithModel("gpt-4o")
	out, err := s.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	if err != nil || len(out) != 1 {
		t.Fatalf("start: %d %v", len(out), err)
	}
	ev := decodeEvent(t, out[0])
	if ev.Message == nil {
		t.Fatalf("message_start must carry message: %s", out[0])
	}
	if ev.Message.StopReason != nil || ev.Message.StopSequence != nil {
		t.Fatalf("stop_reason/stop_sequence must be null: %+v", ev.Message)
	}
	if ev.Message.Usage == nil {
		t.Fatalf("message_start message must carry usage: %+v", ev.Message)
	}
	if !strings.Contains(string(out[0]), `"stop_reason":null`) || !strings.Contains(string(out[0]), `"stop_sequence":null`) {
		t.Fatalf("raw message_start must contain explicit nulls: %s", out[0])
	}
}

// Important 3: message_delta 的 stop_reason 必须嵌套在 delta 内
// （{"delta":{"stop_reason":"end_turn"}}），顶层不得出现 stop_reason。
func TestO2CStream_MessageDeltaNested(t *testing.T) {
	s := NewO2CStreamWithModel("gpt-4o")
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	if len(out) != 2 {
		t.Fatalf("finish frames: %d (%s)", len(out), out)
	}
	ev := decodeEvent(t, out[1])
	if ev.Delta == nil || ev.Delta.StopReason == nil || *ev.Delta.StopReason != "end_turn" {
		t.Fatalf("stop_reason must be nested in delta: %+v", ev)
	}
	if ev.StopReason != nil {
		t.Fatalf("top-level stop_reason must not exist: %+v", ev)
	}
	if !strings.Contains(string(out[1]), `"delta":{"stop_reason":"end_turn"}`) {
		t.Fatalf("raw message_delta shape: %s", out[1])
	}
}

// Important 3: usage-only message_delta 恒带 delta 对象（{}），usage 随帧携带；
// 真实 Claude delta 的 usage 只含 output_tokens（input_tokens 已随 message_start 上报）。
func TestO2CStream_MessageDeltaUsageOnly(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	if len(out) != 1 || !strings.Contains(string(out[0]), "message_delta") {
		t.Fatalf("usage frame: %s", out[0])
	}
	ev := decodeEvent(t, out[0])
	if ev.Delta == nil {
		t.Fatalf("usage-only message_delta must still carry delta: %+v", ev)
	}
	if ev.Usage == nil || ev.Usage.OutputTokens != 5 {
		t.Fatalf("usage payload: %+v", ev.Usage)
	}
	if strings.Contains(string(out[0]), "input_tokens") {
		t.Fatalf("delta usage must not carry input_tokens: %s", out[0])
	}
}

// Important 3: finish chunk 自带 usage → 单个 message_delta 同时携带 delta 与 usage。
func TestO2CStream_FinishUsageMerged(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	if len(out) != 2 {
		t.Fatalf("finish frames: %d (%s)", len(out), out)
	}
	ev := decodeEvent(t, out[1])
	if ev.Delta == nil || ev.Delta.StopReason == nil || *ev.Delta.StopReason != "end_turn" {
		t.Fatalf("delta: %+v", ev)
	}
	if ev.Usage == nil || ev.Usage.InputTokens != 10 || ev.Usage.OutputTokens != 5 {
		t.Fatalf("usage must ride along the finish delta: %+v", ev.Usage)
	}
}

// Minor 5: usage chunk → message_delta{usage}，delta usage 只映射 output_tokens
// （input_tokens 已随 message_start 上报，不重复携带）。
func TestO2CStream_UsageDelta(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	if len(out) != 1 || !strings.Contains(string(out[0]), "message_delta") {
		t.Fatalf("usage frame: %s", out[0])
	}
	ev := decodeEvent(t, out[0])
	if ev.Usage == nil || ev.Usage.OutputTokens != 5 {
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

// reasoning → text：thinking idx 0 → text idx 1，索引连续
func TestO2CStream_ReasoningThenText(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"We"},"finish_reason":null}]}`))
	if len(out) != 2 {
		t.Fatalf("reasoning start frames: %d (%s)", len(out), out)
	}
	start := decodeEvent(t, out[0])
	if start.ContentBlock == nil || start.ContentBlock.Type != "thinking" || start.Index != 0 {
		t.Fatalf("thinking block must be index 0: %+v", start)
	}
	delta := decodeEvent(t, out[1])
	if delta.Delta == nil || delta.Delta.Type != "thinking_delta" || delta.Delta.Thinking != "We" {
		t.Fatalf("thinking delta: %+v", delta.Delta)
	}
	// text 到达 → 关 thinking，开 text idx 1
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	if len(out) != 3 {
		t.Fatalf("reasoning-then-text frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("thinking must close: %s", out[0])
	}
	textStart := decodeEvent(t, out[1])
	if textStart.ContentBlock == nil || textStart.ContentBlock.Type != "text" || textStart.Index != 1 {
		t.Fatalf("text block must be index 1: %+v", textStart)
	}
}

// 纯 reasoning + length finish：thinking 关闭 + message_delta{max_tokens}，无 text 块
func TestO2CStream_ReasoningOnlyLengthFinish(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"deep"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`))
	if len(out) != 2 || !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("thinking must close before message_delta: %s", out)
	}
	if !strings.Contains(string(out[1]), "\"stop_reason\":\"max_tokens\"") {
		t.Fatalf("finish: %s", out[1])
	}
}

// Finish() 时 thinking 未闭合 → content_block_stop + message_stop
func TestO2CStream_FinishClosesThinking(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"deep"},"finish_reason":null}]}`))
	fin := s.Finish()
	if len(fin) != 2 || !strings.Contains(string(fin[0]), "content_block_stop") || !strings.Contains(string(fin[1]), "message_stop") {
		t.Fatalf("finish must close thinking: %s", fin)
	}
}

// 多 chunk 增量 → 连续 thinking_delta 不拆块
func TestO2CStream_ReasoningContinuationNoSplit(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"a"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"b"},"finish_reason":null}]}`))
	if len(out) != 1 {
		t.Fatalf("continuation frames: %d (%s)", len(out), out)
	}
	ev := decodeEvent(t, out[0])
	if ev.Index != 0 || ev.Delta == nil || ev.Delta.Type != "thinking_delta" || ev.Delta.Thinking != "b" {
		t.Fatalf("continuation delta: %+v", ev)
	}
}

// reasoning → tool_calls：thinking idx 0 → tool_use idx 1
func TestO2CStream_ReasoningThenTool(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	if len(out) != 2 || !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("thinking must close before tool start: %s", out)
	}
	toolStart := decodeEvent(t, out[1])
	if toolStart.ContentBlock == nil || toolStart.ContentBlock.Type != "tool_use" || toolStart.Index != 1 {
		t.Fatalf("tool_use must be index 1 after thinking: %+v", toolStart)
	}
}

// 同一 chunk reasoning + content（异常防御）：reasoning 先、text 后
func TestO2CStream_ReasoningAndContentSameChunk(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"r","content":"c"},"finish_reason":null}]}`))
	// [start(thinking,0), thinking_delta, stop(0), start(text,1), text_delta]
	if len(out) != 5 {
		t.Fatalf("same-chunk frames: %d (%s)", len(out), out)
	}
	start := decodeEvent(t, out[0])
	if start.ContentBlock == nil || start.ContentBlock.Type != "thinking" || start.Index != 0 {
		t.Fatalf("frame 0 must be thinking start idx 0: %+v", start)
	}
	thinkDelta := decodeEvent(t, out[1])
	if thinkDelta.Delta == nil || thinkDelta.Delta.Type != "thinking_delta" || thinkDelta.Delta.Thinking != "r" {
		t.Fatalf("frame 1 must be thinking_delta: %+v", thinkDelta.Delta)
	}
	if !strings.Contains(string(out[2]), "content_block_stop") {
		t.Fatalf("frame 2 must be thinking stop: %s", out[2])
	}
	textStart := decodeEvent(t, out[3])
	if textStart.ContentBlock == nil || textStart.ContentBlock.Type != "text" || textStart.Index != 1 {
		t.Fatalf("frame 3 must be text start idx 1: %+v", textStart)
	}
	textDelta := decodeEvent(t, out[4])
	if textDelta.Delta == nil || textDelta.Delta.Type != "text_delta" || textDelta.Delta.Text != "c" {
		t.Fatalf("frame 4 must be text_delta: %+v", textDelta.Delta)
	}
}

// I1: reasoning 增量保留原文——TrimSpace 仅作空判断，不裁剪词间/前导空格
func TestO2CStream_ReasoningKeepsInnerSpaces(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"We"},"finish_reason":null}]}`))
	if len(out) != 2 {
		t.Fatalf("first thinking chunk frames: %d (%s)", len(out), out)
	}
	delta := decodeEvent(t, out[1])
	if delta.Delta == nil || delta.Delta.Thinking != "We" {
		t.Fatalf("first delta must carry original text: %+v", delta.Delta)
	}
	for _, tok := range []string{" think", " about", " it"} {
		out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"` + tok + `"},"finish_reason":null}]}`))
		if len(out) != 1 {
			t.Fatalf("chunk %q frames: %d (%s)", tok, len(out), out)
		}
		ev := decodeEvent(t, out[0])
		if ev.Delta == nil || ev.Delta.Thinking != tok {
			t.Fatalf("delta must carry original %q: %+v", tok, ev.Delta)
		}
	}
}

// I2: text 块打开后到达的 reasoning 增量丢弃（thinking 必须首个且唯一，防御路径）
func TestO2CStream_ReasoningAfterTextDropped(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"late"},"finish_reason":null}]}`))
	if len(out) != 0 {
		t.Fatalf("late reasoning must be dropped, got %d frames: %s", len(out), out)
	}
}

// reasoning 流 Finish() 幂等：首次关 thinking + message_stop，第二次零帧
func TestO2CStream_ReasoningFinishIdempotent(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"r"},"finish_reason":null}]}`))
	first := s.Finish()
	if len(first) != 2 || !strings.Contains(string(first[0]), "content_block_stop") || !strings.Contains(string(first[1]), "message_stop") {
		t.Fatalf("first Finish must close thinking: %s", first)
	}
	if second := s.Finish(); len(second) != 0 {
		t.Fatalf("second Finish must emit nothing, got %d frames", len(second))
	}
}
