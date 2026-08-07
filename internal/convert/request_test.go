package convert

import "testing"

func TestOpenAIRequestToClaude(t *testing.T) {
	n := 2
	req := &ChatCompletionRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: FunctionDef{Name: "f", Parameters: map[string]any{"type": "object"}}}},
		N:        &n,
	}
	_, err := OpenAIRequestToClaude(req, "claude-3")
	if err == nil {
		t.Fatal("n>1 must be rejected")
	}
	req.N = nil
	out, err := OpenAIRequestToClaude(req, "claude-3")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-3" || out.MaxTokens != 4096 {
		t.Fatalf("model/max_tokens: %+v", out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "f" {
		t.Fatalf("tools: %+v", out.Tools)
	}
}

func TestClaudeRequestToOpenAI(t *testing.T) {
	req := &MessagesRequest{
		Model:         "claude-3",
		System:        "sys",
		MaxTokens:     2048,
		Messages:      []ClaudeMessage{{Role: "user", Content: "hi"}},
		Tools:         []ClaudeTool{{Name: "f", InputSchema: map[string]any{"type": "object"}}},
		StopSequences: []string{"\n"},
	}
	out, err := ClaudeRequestToOpenAI(req, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-4o" || *out.MaxTokens != 2048 {
		t.Fatalf("model/max_tokens: %+v", out)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "system" || out.Messages[0].Content != "sys" {
		t.Fatalf("messages: %+v", out.Messages)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "f" {
		t.Fatalf("tools: %+v", out.Tools)
	}
}

func TestClaudeRequestToOpenAI_SystemArray(t *testing.T) {
	req := &MessagesRequest{
		Model:     "claude-3",
		System:    []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}},
		MaxTokens: 1024,
		Messages:  []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	out, err := ClaudeRequestToOpenAI(req, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "system" || out.Messages[0].Content != "a\nb" {
		t.Fatalf("messages: %+v", out.Messages)
	}
}
