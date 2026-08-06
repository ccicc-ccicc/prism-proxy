package convert

import (
	"strings"
	"testing"
)

func TestOpenAIResponseToClaude(t *testing.T) {
	resp := &ChatCompletionResponse{
		Choices: []ResponseChoice{{Message: ChatMessage{
			Content:   "hi",
			ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}},
		}, FinishReason: "tool_calls"}},
		Usage: &Usage{PromptTokens: 10, CompletionTokens: 5},
	}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "msg_") || len(out.Content) != 2 {
		t.Fatalf("id/content: %+v", out)
	}
	if out.Content[1].Type != "tool_use" || out.Content[1].Name != "f" {
		t.Fatalf("tool_use: %+v", out.Content[1])
	}
	if out.Content[1].Input.(map[string]any)["x"] != float64(1) {
		t.Fatalf("input parsed: %+v", out.Content[1].Input)
	}
	if out.StopReason == nil || *out.StopReason != "tool_use" {
		t.Fatalf("stop_reason: %v", out.StopReason)
	}
	if out.Usage == nil || out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Fatalf("usage: %+v", out.Usage)
	}
}

func TestClaudeResponseToOpenAI_ThinkingStripped(t *testing.T) {
	resp := &MessagesResponse{
		Content: []ClaudeBlock{
			{Type: "thinking", Thinking: "secret"},
			{Type: "text", Text: "answer"},
			{Type: "tool_use", ID: "toolu_1", Name: "f", Input: map[string]any{"x": 1}},
		},
		StopReason: strPtr("tool_use"),
	}
	out, err := ClaudeResponseToOpenAI(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Fatalf("id: %s", out.ID)
	}
	if out.Choices[0].Message.Content != "answer" {
		t.Fatalf("content: %v", out.Choices[0].Message.Content)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 || out.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("tool_calls: %+v", out.Choices[0].Message.ToolCalls)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason: %s", out.Choices[0].FinishReason)
	}
}

func strPtr(s string) *string { return &s }
