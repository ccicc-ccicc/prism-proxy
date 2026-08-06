package convert

import (
	"strings"
)

func OpenAIResponseToClaude(resp *ChatCompletionResponse, id string) (*MessagesResponse, error) {
	if id == "" {
		id = NewClaudeID()
	}
	out := &MessagesResponse{ID: id, Type: "message", Role: "assistant", Model: resp.Model}
	var blocks []ClaudeBlock
	for _, ch := range resp.Choices {
		if s := contentString(ch.Message.Content); s != "" {
			blocks = append(blocks, ClaudeBlock{Type: "text", Text: s})
		}
		for _, tc := range ch.Message.ToolCalls {
			input, perr := OpenAIArgsToClaudeInput(tc.Function.Arguments)
			if perr != nil {
				input = map[string]any{}
			}
			blocks = append(blocks, ClaudeBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
		}
	}
	out.Content = blocks
	if len(resp.Choices) > 0 {
		if fr := resp.Choices[0].FinishReason; fr != "" {
			out.StopReason = stopReasonO2C(fr)
		}
	}
	if resp.Usage != nil {
		out.Usage = &ClaudeUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	}
	return out, nil
}

func ClaudeResponseToOpenAI(resp *MessagesResponse, id string) (*ChatCompletionResponse, error) {
	if id == "" {
		id = NewOpenAIID()
	}
	out := &ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: 0,
		Model:   resp.Model,
	}
	var text strings.Builder
	var toolCalls []ToolCall
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args, err := ClaudeInputToOpenAIArgs(b.Input)
			if err != nil {
				args = "{}"
			}
			toolCalls = append(toolCalls, ToolCall{ID: b.ID, Type: "function", Function: ToolCallFunction{Name: b.Name, Arguments: args}})
		case "image":
			if b.Source == nil || b.Source.MediaType == "" || b.Source.Data == "" {
				continue // 防御：source 缺失/不完整 → 跳过，不 panic、不产生非法 data URL
			}
			text.WriteString(ClaudeSourceToImageURL(b.Source))
		case "thinking", "redacted_thinking", "signature":
			// 剥离
		}
	}
	msg := ChatMessage{Role: "assistant", Content: text.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	fr := "stop"
	if resp.StopReason != nil {
		fr = finishReasonC2O(*resp.StopReason)
	}
	out.Choices = []ResponseChoice{{Index: 0, Message: msg, FinishReason: fr}}
	if resp.Usage != nil {
		out.Usage = &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}
	return out, nil
}
