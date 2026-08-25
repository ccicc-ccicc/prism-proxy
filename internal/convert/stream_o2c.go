package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// O2CStream 将 OpenAI SSE chunk JSON 转换为 Claude SSE 帧。
//
// 事件序列遵循 spec §5 的完整 Claude 流式协议：
// message_start → content_block_start → content_block_delta* → content_block_stop
// → (下一块) → message_delta → message_stop。
//
// 块语义：
//   - text 块内所有内容 delta 连续发射，不做逐 delta 的 stop 拆分；
//   - 开始任何新块（text 或 tool_use）前，若已有块打开，先发该块的
//     content_block_stop 并全局 blockIdx++，保证 Claude content_block index
//     从 0 连续（text=0、tool_use=1…；纯 tool 流第一块为 0）；
//   - tool_calls start chunk 自带的 arguments 在 block start 后作为
//     input_json_delta 发射；
//   - finish_reason 到达时先关闭打开的块，再发 message_delta；
//   - Finish() 幂等：仅首次调用产出帧。
type O2CStream struct {
	id         string
	model      string
	started    bool
	blockIdx   int // Claude content_block 全局索引（text=0、tool_use=1…）
	inText     bool
	inTool     bool
	inThinking bool
	sentStop   bool
	done       bool
}

func NewO2CStream() *O2CStream { return NewO2CStreamWithModel("") }

func NewO2CStreamWithModel(model string) *O2CStream {
	return &O2CStream{id: NewClaudeID(), model: model}
}

func (s *O2CStream) Write(data []byte) ([][]byte, error) {
	var chunk StreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("o2c parse chunk: %w", err)
	}
	var frames [][]byte
	if !s.started {
		s.started = true
		frames = append(frames, s.frame(EventMessageStart, StreamEvent{
			// 真实 Anthropic message_start 的 message 携带 stop_reason/stop_sequence
			// （null）与 usage（零值），SDK 按此形状解析。
			Message: &MessagesResponse{ID: s.id, Type: "message", Role: "assistant", Model: s.model, Content: []ClaudeBlock{}, Usage: &ClaudeUsage{}},
		}))
	}
	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			// message_delta 恒带 delta 对象（usage-only 时为 {}）；真实 Claude delta 的
			// usage 仅携带 output_tokens（input_tokens 已随 message_start 上报）
			frames = append(frames, s.frame(EventMessageDelta, StreamEvent{Delta: &ClaudeDelta{}, Usage: &ClaudeUsage{OutputTokens: chunk.Usage.CompletionTokens}}))
		}
		return frames, nil
	}
	delta := chunk.Choices[0].Delta
	// reasoning_content 逐 chunk 增量下发（deepseek 系行为）；若上游改为
	// 全量下发，需先拼接再发射，避免重复拼接。
	if rc := strings.TrimSpace(delta.ReasoningContent); rc != "" {
		if s.inText || s.inTool {
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText, s.inTool = false, false
			s.blockIdx++
		}
		if !s.inThinking {
			s.inThinking = true
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "thinking"}}))
		}
		frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "thinking_delta", Thinking: rc}}))
	}
	if text, ok := delta.Content.(string); ok && text != "" {
		if s.inTool || s.inThinking {
			// 块切换（tool_use/thinking → text）：先关当前块并递增 index
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText, s.inTool, s.inThinking = false, false, false
			s.blockIdx++
		}
		if !s.inText && !s.inTool && !s.inThinking {
			s.inText = true
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "text"}}))
		}
		frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "text_delta", Text: text}}))
	}
	for _, tc := range delta.ToolCalls {
		if tc.ID != "" && tc.Function.Name != "" {
			if s.inText || s.inTool || s.inThinking {
				frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
				s.inText, s.inTool, s.inThinking = false, false, false
				s.blockIdx++
			}
			s.inTool = true
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: map[string]any{}}}))
			if tc.Function.Arguments != "" {
				frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments}}))
			}
			continue
		}
		if s.inTool && tc.Function.Arguments != "" {
			frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments}}))
		}
	}
	if fr := chunk.Choices[0].FinishReason; fr != nil && !s.sentStop {
		s.sentStop = true
		if s.inText || s.inTool || s.inThinking {
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText, s.inTool, s.inThinking = false, false, false
		}
		// Anthropic 真实 message_delta：stop_reason 嵌套在 delta 内
		// （{"delta":{"stop_reason":"end_turn"}}），usage 可同帧携带
		ev := StreamEvent{Delta: &ClaudeDelta{StopReason: stopReasonO2C(*fr)}}
		if chunk.Usage != nil {
			ev.Usage = &ClaudeUsage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		frames = append(frames, s.frame(EventMessageDelta, ev))
	} else if chunk.Usage != nil {
		// 无 stop_reason 的 usage 帧：delta usage 仅携带 output_tokens
		frames = append(frames, s.frame(EventMessageDelta, StreamEvent{Delta: &ClaudeDelta{}, Usage: &ClaudeUsage{OutputTokens: chunk.Usage.CompletionTokens}}))
	}
	return frames, nil
}

func (s *O2CStream) Finish() [][]byte {
	if s.done {
		return nil
	}
	s.done = true
	if s.inText || s.inTool || s.inThinking {
		s.inText, s.inTool, s.inThinking = false, false, false
		return [][]byte{s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}), s.frame(EventMessageStop, StreamEvent{})}
	}
	return [][]byte{s.frame(EventMessageStop, StreamEvent{})}
}

func stopReasonO2C(reason string) *string {
	var r string
	switch reason {
	case "stop":
		r = "end_turn"
	case "tool_calls":
		r = "tool_use"
	case "length":
		r = "max_tokens"
	default:
		r = "end_turn"
	}
	return &r
}

func (s *O2CStream) frame(event string, payload StreamEvent) []byte {
	// 真实 Anthropic 帧的 data 恒携带 type 字段（等于事件名）
	payload.Type = event
	b, _ := json.Marshal(payload)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}
