package convert

import (
	"strings"
	"testing"
)

func TestOpenAIMessagesToClaude_SystemMerge(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: "a"},
		{Role: "system", Content: "b"},
		{Role: "user", Content: "hi"},
	}
	system, out, err := OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if system != "a\nb" {
		t.Fatalf("system: %q", system)
	}
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("out: %+v", out)
	}
}

func TestOpenAIMessagesToClaude_ToolCalls(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
	}
	_, out, err := OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Role != "user" {
		t.Fatalf("out: %+v", out)
	}
	blocks := out[1].Content.([]ClaudeBlock)
	if blocks[0].Type != "tool_result" || blocks[0].ToolUseID == "" || blocks[0].Content != "result" {
		t.Fatalf("tool_result block: %+v", blocks[0])
	}
	// 验证非法 arguments 兜底
	msgs[0].ToolCalls[0].Function.Arguments = "{not json"
	_, _, err = OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatalf("invalid args should not fail: %v", err)
	}
}

func TestClaudeMessagesToOpenAI_ToolResultOrder(t *testing.T) {
	// user 消息内 tool_result + 文本 → tool 消息必须在前
	msgs := []ClaudeMessage{
		{Role: "user", Content: []ClaudeBlock{
			{Type: "tool_result", Content: "r"},
			{Type: "text", Text: "and also"},
		}},
	}
	out, err := ClaudeMessagesToOpenAI(msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Role != "tool" || out[1].Role != "user" {
		t.Fatalf("order: %+v", out)
	}
}

func TestImageConversions(t *testing.T) {
	src, rest, err := ImageURLToClaudeSource("data:image/png;base64,AAAA")
	if err != nil || rest != "" || src.MediaType != "image/png" || src.Data != "AAAA" {
		t.Fatalf("data url: %+v %q %v", src, rest, err)
	}
	_, rest, _ = ImageURLToClaudeSource("https://x.com/a.png")
	if rest != "https://x.com/a.png" {
		t.Fatalf("external: %q", rest)
	}
	url := ClaudeSourceToImageURL(&ImageSource{Type: "base64", MediaType: "image/jpeg", Data: "BBBB"})
	if url != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("url: %q", url)
	}
}

func TestIDs(t *testing.T) {
	if !strings.HasPrefix(NewOpenAIID(), "chatcmpl-") || !strings.HasPrefix(NewClaudeID(), "msg_") {
		t.Fatal("id prefixes")
	}
}

func TestBlockHasImage_NestedInToolResult(t *testing.T) {
	blocks := []ClaudeBlock{
		{Type: "tool_result", Content: []ClaudeBlock{{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "x"}}}},
	}
	if !BlockHasImage(blocks) {
		t.Fatal("image nested in tool_result not detected")
	}
}
