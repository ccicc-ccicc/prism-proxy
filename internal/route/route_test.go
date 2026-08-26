package route

import (
	"encoding/json"
	"testing"

	"prism-proxy/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main":   {BaseURL: "https://a/v1", APIKey: "k", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: "https://b/v1", APIKey: "k", Format: "claude", Model: "claude-3"},
		},
	}
}

func TestDecide_VisionSwitch(t *testing.T) {
	body := []byte(`{"model":"ignored","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(testConfig(), "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if !d.VisionSwitch || d.Upstream != "vision" || d.Model != "claude-3" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_TextGoesMain(t *testing.T) {
	body := []byte(`{"model":"whatever","messages":[{"role":"user","content":"hi"}]}`)
	d, err := Decide(testConfig(), "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.VisionSwitch || d.Upstream != "main" || d.Model != "gpt-4o" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_ClaudeFormatImage(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	d, err := Decide(testConfig(), "claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if !d.VisionSwitch || d.Upstream != "vision" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_ImageInToolResult(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`)
	has, err := RequestHasImage("claude", body)
	if err != nil || !has {
		t.Fatalf("tool_result nested image: %v %v", has, err)
	}
}

func TestDecide_Disabled(t *testing.T) {
	cfg := testConfig()
	cfg.AutoSwitchVision = false
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(cfg, "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Upstream != "main" || d.VisionSwitch {
		t.Fatalf("decision: %+v", d)
	}
}

// TestDecide_VisionMissing: auto_switch_vision 开启但无 vision 上游（配置直接构造，
// 绕过 Validate 的 fail-fast）时，带图请求应回落 main，不 panic。
func TestDecide_VisionMissing(t *testing.T) {
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "https://a/v1", APIKey: "k", Format: "openai", Model: "gpt-4o"},
		},
	}
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(cfg, "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Upstream != "main" || d.VisionSwitch || d.Model != "gpt-4o" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_InvalidBody(t *testing.T) {
	_, err := Decide(testConfig(), "openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}

// thinkingToolUseBody 顶层 thinking 参数 + 缺 thinking 块的 assistant(tool_use)（核心触发场景）。
const thinkingToolUseBody = `{"model":"x","thinking":{"type":"enabled","budget_tokens":1024},"messages":[
	{"role":"user","content":"查一下"},
	{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search","input":{"q":"x"}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"nothing"}]}
]}`

// thinkingWithToolUseBody 顶层 thinking 参数 + 已含 thinking 块的 assistant(tool_use)（不触发补块）。
const thinkingWithToolUseBody = `{"model":"x","thinking":{"type":"enabled","budget_tokens":1024},"messages":[
	{"role":"assistant","content":[{"type":"thinking","thinking":"考虑中","signature":"abc"},{"type":"tool_use","id":"t1","name":"search","input":{}}]}
]}`

// historyThinkingBody 历史含 thinking 块（无顶层参数）+ 后段 assistant(tool_use) 缺 thinking。
const historyThinkingBody = `{"model":"x","messages":[
	{"role":"user","content":"看图"},
	{"role":"assistant","content":[{"type":"thinking","thinking":"分析中"},{"type":"text","text":"好的"}]},
	{"role":"user","content":"再查一次"},
	{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"search","input":{}}]}
]}`

// alreadyPatchedBody 补过一次后的形态：空 thinking 块打头 + tool_use（幂等，不再补）。
const alreadyPatchedBody = `{"model":"x","thinking":{"type":"enabled","budget_tokens":1024},"messages":[
	{"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"tool_use","id":"t1","name":"search","input":{}}]}
]}`

func TestEnsureThinkingBlocks(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		body       []byte
		wantErr    bool
		wantChange bool
		check      func(t *testing.T, out []byte)
	}{
		{
			name:       "top-level thinking + tool_use without thinking → patch",
			format:     "claude",
			body:       []byte(thinkingToolUseBody),
			wantChange: true,
			check: func(t *testing.T, out []byte) {
				var m map[string]any
				_ = json.Unmarshal(out, &m)
				content := m["messages"].([]any)[1].(map[string]any)["content"].([]any)
				first := content[0].(map[string]any)
				if first["type"] != "thinking" || first["thinking"] != "" {
					t.Fatalf("patched block: %v", first)
				}
				// tool_use 保留在后续位置
				if len(content) != 2 || content[1].(map[string]any)["type"] != "tool_use" {
					t.Fatalf("tool_use not preserved: %v", content)
				}
			},
		},
		{
			name:       "assistant already has thinking + tool_use → untouched",
			format:     "claude",
			body:       []byte(thinkingWithToolUseBody),
			wantChange: false,
		},
		{
			name:       "history thinking block triggers patch on later tool_use",
			format:     "claude",
			body:       []byte(historyThinkingBody),
			wantChange: true,
			check: func(t *testing.T, out []byte) {
				var m map[string]any
				_ = json.Unmarshal(out, &m)
				content := m["messages"].([]any)[3].(map[string]any)["content"].([]any)
				if content[0].(map[string]any)["type"] != "thinking" || content[0].(map[string]any)["thinking"] != "" {
					t.Fatalf("expected patched thinking block: %v", content)
				}
			},
		},
		{
			name:       "no thinking mode → noop",
			format:     "claude",
			body:       []byte(`{"model":"x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search","input":{}}]}]}`),
			wantChange: false,
		},
		{
			name:       "assistant text only without tool_use → untouched",
			format:     "claude",
			body:       []byte(`{"model":"x","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`),
			wantChange: false,
		},
		{
			name:       "already patched empty thinking block → noop (idempotent)",
			format:     "claude",
			body:       []byte(alreadyPatchedBody),
			wantChange: false,
		},
		{
			name:    "invalid body → error",
			format:  "claude",
			body:    []byte("{not json"),
			wantErr: true,
		},
		{
			name:       "openai format → noop",
			format:     "openai",
			body:       []byte(`{"model":"x","messages":[{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"s","arguments":"{}"}}]}]}`),
			wantChange: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, err := EnsureThinkingBlocks(tt.format, tt.body)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChange {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChange)
			}
			if tt.check != nil {
				tt.check(t, out)
			}
			if !tt.wantChange && string(out) != string(tt.body) {
				t.Fatalf("no-op body not byte-identical:\n got: %s", out)
			}
		})
	}
	// 幂等：补过一次的真实输出再调用 → no-op，且逐字节一致
	patched, changed, err := EnsureThinkingBlocks("claude", []byte(thinkingToolUseBody))
	if err != nil || !changed {
		t.Fatalf("first patch: %v %v", changed, err)
	}
	again, changed, err := EnsureThinkingBlocks("claude", patched)
	if err != nil || changed {
		t.Fatalf("second run must be noop: %v %v", changed, err)
	}
	if string(again) != string(patched) {
		t.Fatal("second run body not byte-identical")
	}
}
