package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// C2OStream 将 Claude SSE 帧转换为 OpenAI SSE chunk（spec §5 的 C2O 绑定）。
//
// 事件映射：
//   - message_start → 首个 chunk（delta.role=assistant、content="");
//   - text 块 start/stop 静默，text_delta → delta.content；
//   - tool_use 块 start → delta.tool_calls[{index:<toolIdx>, id, type:function,
//     function{name, arguments:""}}]，toolIdx 独立计数只对 tool_use；
//   - input_json_delta → delta.tool_calls[{index:<toolIdx>, function{arguments}}]；
//   - message_delta stop_reason → finish_reason（end_turn→stop、tool_use→tool_calls、
//     max_tokens→length、其余→stop）；usage → 并入最近一个已输出的 chunk，
//     若从未输出 chunk 则吞掉；
//   - ping/stats/未知事件吞掉；thinking/redacted_thinking 块完全剥离；
//   - error 事件 → 返回 error 终止流（不伪造 [DONE]）；
//   - message_stop → 无输出；Close() 返回 data: [DONE]\n\n。
type C2OStream struct {
	id        string
	model     string
	toolIdx   int // 下一个 tool_use 块的索引（只对 tool_use 独立计数）
	inTool    bool
	lastChunk []byte
	stopped   bool // 是否收到 message_stop（上游完整结束标志）
}

func NewC2OStream(model string) *C2OStream {
	return &C2OStream{id: NewOpenAIID(), model: model}
}

// ParseClaudeFrame 解析一条 Claude SSE 帧（event: <type>\ndata: <json>\n\n）。
func ParseClaudeFrame(frame []byte) (event string, data []byte, err error) {
	lines := bytes.Split(bytes.TrimRight(frame, "\n"), []byte("\n"))
	for _, ln := range lines {
		if bytes.HasPrefix(ln, []byte("event: ")) {
			event = string(bytes.TrimPrefix(ln, []byte("event: ")))
		}
		if bytes.HasPrefix(ln, []byte("data: ")) {
			data = bytes.TrimPrefix(ln, []byte("data: "))
		}
	}
	if event == "" {
		return "", nil, fmt.Errorf("claude frame missing event: %q", frame)
	}
	return event, data, nil
}

func (s *C2OStream) Write(frame []byte) ([][]byte, error) {
	event, data, err := ParseClaudeFrame(frame)
	if err != nil {
		return nil, err
	}
	switch event {
	case EventMessageStart:
		// 首个 chunk：role=assistant、content=""（finish_reason 缺失即 null）
		return [][]byte{s.chunk(StreamChoice{Delta: ChatMessage{Role: "assistant", Content: ""}})}, nil
	case EventPing, EventContentBlockStop:
		// content_block_stop 同时复位 inTool，保证块切换后（tool_use → text）
		// 后续 text_delta 不被误吞。
		s.inTool = false
		return nil, nil
	case EventError:
		// 上游错误事件：立即终止流（返回 error）。readClaudeFrames 收到 error
		// 后不再回调 onDone，因此下游拿不到 [DONE]/finish_reason——绝不把
		// 截断的流伪装成成功（spec §5 "不伪造结束事件"）。
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("upstream stream error (unparseable payload)")
		}
		if ev.Error != nil && ev.Error.Message != "" {
			return nil, fmt.Errorf("upstream stream error: %s", ev.Error.Message)
		}
		return nil, fmt.Errorf("upstream stream error")
	case EventContentBlockStart:
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		if ev.ContentBlock == nil {
			return nil, nil
		}
		switch ev.ContentBlock.Type {
		case "thinking", "redacted_thinking":
			return nil, nil // 剥离
		case "tool_use":
			// 无条件独立递增：每个真实 tool 块都以 content_block_stop 结束
			// （复位 inTool），不能用 inTool 守卫判断新块。
			idx := s.toolIdx
			s.toolIdx++
			s.inTool = true
			chunk := s.chunk(StreamChoice{
				Delta: ChatMessage{ToolCalls: []ToolCall{{
					Index:    &idx,
					ID:       ev.ContentBlock.ID,
					Type:     "function",
					Function: ToolCallFunction{Name: ev.ContentBlock.Name, Arguments: ""},
				}}},
			})
			return [][]byte{chunk}, nil
		default: // text 等：静默
			return nil, nil
		}
	case EventContentBlockDelta:
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		if ev.Delta == nil {
			return nil, nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if s.inTool { // 防御：text delta 不应出现在 tool 中
				return nil, nil
			}
			return [][]byte{s.chunk(StreamChoice{Delta: ChatMessage{Content: ev.Delta.Text}})}, nil
		case "input_json_delta":
			if s.toolIdx == 0 { // 防御：无 tool 块时不应出现参数增量
				return nil, nil
			}
			idx := s.toolIdx - 1 // 当前（最近开始）tool 块的索引
			return [][]byte{s.chunk(StreamChoice{Delta: ChatMessage{ToolCalls: []ToolCall{{Index: &idx, Function: ToolCallFunction{Arguments: ev.Delta.PartialJSON}}}}})}, nil
		case "thinking_delta", "signature_delta":
			return nil, nil // 剥离
		}
		return nil, nil
	case EventMessageDelta:
		// 真实 Claude 帧中 stop_reason 嵌套在 delta 内（顶层 stop_reason 无值），
		// 复用扩展后的 ClaudeDelta 精确捕获 delta.stop_reason 与顶层 usage。
		var ev struct {
			Delta *ClaudeDelta `json:"delta"`
			Usage *ClaudeUsage `json:"usage"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		var out [][]byte
		if ev.Delta != nil && ev.Delta.StopReason != nil {
			fr := finishReasonC2O(*ev.Delta.StopReason)
			out = append(out, s.chunk(StreamChoice{FinishReason: &fr}))
		}
		if ev.Usage != nil {
			u := &Usage{
				PromptTokens:     ev.Usage.InputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
			}
			if len(out) > 0 {
				out[len(out)-1] = s.withUsage(out[len(out)-1], u)
			} else if s.lastChunk != nil {
				// 纯 usage delta：并入最近一个已输出的 chunk（重发该 chunk）
				out = append(out, s.withUsage(s.lastChunk, u))
			}
		}
		return out, nil
	case EventMessageStop:
		s.stopped = true
		return nil, nil
	default:
		// 未知事件（含 stats）：吞掉
		return nil, nil
	}
}

func finishReasonC2O(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// Stopped 报告是否已收到 message_stop。上游未发 message_stop 即 EOF 属截断流，
// 调用方不得回写 [DONE]（不伪造结束事件）。
func (s *C2OStream) Stopped() bool { return s.stopped }

func (s *C2OStream) chunk(choice StreamChoice) []byte {
	b, _ := json.Marshal(StreamChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(), // 真实时间戳（非 0）
		Model:   s.model,
		Choices: []StreamChoice{{Index: 0, Delta: choice.Delta, FinishReason: choice.FinishReason}},
	})
	s.lastChunk = append([]byte("data: "), append(b, '\n', '\n')...)
	return s.lastChunk
}

// withUsage 将 usage 并入已输出的 chunk（反序列化为 StreamChunk 后重建 data 行）。
func (s *C2OStream) withUsage(chunk []byte, u *Usage) []byte {
	line := bytes.TrimSuffix(bytes.TrimPrefix(chunk, []byte("data: ")), []byte("\n\n"))
	var sc StreamChunk
	_ = json.Unmarshal(line, &sc)
	sc.Usage = u
	b := []byte("data: " + mustJSON(sc) + "\n\n")
	s.lastChunk = b
	return b
}

func (s *C2OStream) Close() [][]byte {
	return [][]byte{[]byte("data: [DONE]\n\n")}
}
