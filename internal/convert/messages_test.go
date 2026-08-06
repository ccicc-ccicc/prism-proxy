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

func TestOpenAIMessagesToClaude_ToolResultBeforeText(t *testing.T) {
	// 协议违规交错输入：user 文本夹在 assistant(tool_calls) 与 tool 结果之间。
	// 契约：全部 tool_result 消息先于剩余 user 文本。
	msgs := []ChatMessage{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{}`}}}},
		{Role: "user", Content: "text"},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
	}
	_, out, err := OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("out: %+v", out)
	}
	if out[0].Role != "assistant" || out[1].Role != "user" || out[2].Role != "user" {
		t.Fatalf("roles: %+v", out)
	}
	blocks, ok := out[1].Content.([]ClaudeBlock)
	if !ok || len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "call_1" {
		t.Fatalf("tool_result message: %+v", out[1])
	}
	if out[2].Content != "text" {
		t.Fatalf("text message: %+v", out[2])
	}
}

func TestClaudeMessagesToOpenAI_Contract(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name string
		msgs []ClaudeMessage
		want []ChatMessage
	}{
		{
			name: "system first",
			msgs: []ClaudeMessage{{Role: "user", Content: "hi"}},
			want: []ChatMessage{{Role: "user", Content: "hi"}},
		},
		{
			name: "tool_use to tool_calls",
			msgs: []ClaudeMessage{{Role: "assistant", Content: []ClaudeBlock{{Type: "tool_use", ID: "call_1", Name: "f", Input: map[string]any{"x": float64(1)}}}}},
			want: []ChatMessage{{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}}}},
		},
		{
			name: "tool_use from json shape",
			msgs: []ClaudeMessage{{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "call_2", "name": "g", "input": map[string]any{"y": float64(2)}}}}},
			want: []ChatMessage{{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "g", Arguments: `{"y":2}`}}}}},
		},
		{
			name: "tool_result error prefix",
			msgs: []ClaudeMessage{{Role: "user", Content: []ClaudeBlock{{Type: "tool_result", ToolUseID: "c1", Content: "boom", IsError: boolPtr(true)}}}},
			want: []ChatMessage{{Role: "tool", ToolCallID: "c1", Content: "[error] boom"}},
		},
		{
			name: "tool_result array content concatenated",
			msgs: []ClaudeMessage{{Role: "user", Content: []ClaudeBlock{{Type: "tool_result", ToolUseID: "c1", Content: []ClaudeBlock{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}}}}},
			want: []ChatMessage{{Role: "tool", ToolCallID: "c1", Content: "ab"}},
		},
		{
			name: "multiple tool results",
			msgs: []ClaudeMessage{{Role: "user", Content: []ClaudeBlock{{Type: "tool_result", ToolUseID: "c1", Content: "r1"}, {Type: "tool_result", ToolUseID: "c2", Content: "r2"}}}},
			want: []ChatMessage{{Role: "tool", ToolCallID: "c1", Content: "r1"}, {Role: "tool", ToolCallID: "c2", Content: "r2"}},
		},
		{
			name: "thinking stripped",
			msgs: []ClaudeMessage{{Role: "assistant", Content: []ClaudeBlock{{Type: "thinking", Thinking: "private"}, {Type: "redacted_thinking", Thinking: "secret"}, {Type: "text", Text: "hi"}}}},
			want: []ChatMessage{{Role: "assistant", Content: "hi"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ClaudeMessagesToOpenAI(tc.msgs, "sys")
			if err != nil {
				t.Fatal(err)
			}
			// 每例断言 system 放 messages[0]，其余与 want 逐条比对
			if len(out) == 0 || out[0].Role != "system" || out[0].Content != "sys" {
				t.Fatalf("system first: %+v", out)
			}
			out = out[1:]
			if len(out) != len(tc.want) {
				t.Fatalf("len: got %d want %d: %+v", len(out), len(tc.want), out)
			}
			for i := range out {
				got, want := out[i], tc.want[i]
				if got.Role != want.Role || got.ToolCallID != want.ToolCallID || got.Content != want.Content {
					t.Fatalf("msg %d: got %+v want %+v", i, got, want)
				}
				if len(got.ToolCalls) != len(want.ToolCalls) {
					t.Fatalf("msg %d toolcalls: got %+v want %+v", i, got.ToolCalls, want.ToolCalls)
				}
				for j := range want.ToolCalls {
					g, w := got.ToolCalls[j], want.ToolCalls[j]
					if g.ID != w.ID || g.Type != w.Type || g.Function.Name != w.Function.Name || g.Function.Arguments != w.Function.Arguments {
						t.Fatalf("msg %d tc %d: got %+v want %+v", i, j, g, w)
					}
				}
			}
		})
	}
}

func TestImageConversions(t *testing.T) {
	src, rest, err := ImageURLToClaudeSource("data:image/png;base64,AAAA")
	if err != nil || rest != "" || src.MediaType != "image/png" || src.Data != "AAAA" {
		t.Fatalf("data url: %+v %q %v", src, rest, err)
	}
	// media type 带参数的 data URL：取第一个 ";" 前段
	src, rest, err = ImageURLToClaudeSource("data:image/png;charset=utf-8;base64,AAAA")
	if err != nil || rest != "" || src.MediaType != "image/png" || src.Data != "AAAA" {
		t.Fatalf("data url with params: %+v %q %v", src, rest, err)
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
