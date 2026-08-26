package convert

import (
	"encoding/json"
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
			{Type: "thinking", Thinking: strPtr("secret")},
			{Type: "text", Text: strPtr("answer")},
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

// Minor 8: 空 choices 不 panic（Choices[0] 越界防护）。
func TestOpenAIResponseToClaude_EmptyChoices(t *testing.T) {
	out, err := OpenAIResponseToClaude(&ChatCompletionResponse{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 0 || out.StopReason != nil {
		t.Fatalf("empty choices must yield empty content and nil stop_reason: %+v", out)
	}
}

// Minor 8: image block 的 Source 为 nil/不完整 → 跳过，不 panic、不产生非法 data URL。
func TestClaudeResponseToOpenAI_NilImageSource(t *testing.T) {
	resp := &MessagesResponse{
		Content: []ClaudeBlock{
			{Type: "image"},
			{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png"}}, // 缺 data
			{Type: "text", Text: strPtr("answer")},
		},
	}
	out, err := ClaudeResponseToOpenAI(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content != "answer" {
		t.Fatalf("content: %v", out.Choices[0].Message.Content)
	}
}

func TestOpenAIResponseToClaude_ReasoningMapped(t *testing.T) {
	resp := &ChatCompletionResponse{
		Choices: []ResponseChoice{{Message: ChatMessage{
			Content:          "hi",
			ReasoningContent: "think step by step",
			ToolCalls:        []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}},
		}, FinishReason: "tool_calls"}},
	}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 3 {
		t.Fatalf("want 3 blocks, got %d: %+v", len(out.Content), out.Content)
	}
	if out.Content[0].Type != "thinking" || *out.Content[0].Thinking != "think step by step" {
		t.Fatalf("first block must be thinking: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || *out.Content[1].Text != "hi" {
		t.Fatalf("second block must be text: %+v", out.Content[1])
	}
	if out.Content[2].Type != "tool_use" {
		t.Fatalf("third block must be tool_use: %+v", out.Content[2])
	}
}

func TestOpenAIResponseToClaude_ReasoningOnly(t *testing.T) {
	resp := &ChatCompletionResponse{Choices: []ResponseChoice{{Message: ChatMessage{Content: "", ReasoningContent: "only thinking"}}}}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "thinking" || *out.Content[0].Thinking != "only thinking" {
		t.Fatalf("want only thinking: %+v", out.Content)
	}
}

// Important 1: 多 choice 防御——thinking 仅一次且在最前（取自 Choices[0]）
func TestOpenAIResponseToClaude_MultiChoiceThinkingOnce(t *testing.T) {
	resp := &ChatCompletionResponse{
		Choices: []ResponseChoice{
			{Message: ChatMessage{Content: "a", ReasoningContent: "r1"}},
			{Message: ChatMessage{Content: "b", ReasoningContent: "r2"}},
		},
	}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 3 {
		t.Fatalf("want 3 blocks: %+v", out.Content)
	}
	if out.Content[0].Type != "thinking" || *out.Content[0].Thinking != "r1" {
		t.Fatalf("thinking once first: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[2].Type != "text" {
		t.Fatalf("texts follow: %+v", out.Content)
	}
}

// 回归：不含 reasoning 的 ChatMessage 序列化不得带 reasoning_content 键（C2O 出站安全）
func TestChatMessageMarshalNoReasoningContent(t *testing.T) {
	b, err := json.Marshal(ChatMessage{Role: "assistant", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reasoning_content") {
		t.Fatalf("unexpected reasoning_content key: %s", b)
	}
}
