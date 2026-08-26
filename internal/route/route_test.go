package route

import (
	"encoding/json"
	"strings"
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
	has, err := LatestUserRunHasImage("claude", body)
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

func TestDecide_VisionPreprocess(t *testing.T) {
	// VisionPreprocess=true + 最新 run 含图 + 有 vision 上游 → 走 main 且标记预处理
	cfg := testConfig()
	cfg.VisionPreprocess = true
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(cfg, "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if !d.NeedPreprocess || d.Upstream != "main" || d.Model != "gpt-4o" || d.VisionSwitch {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_VisionPreprocessDisabled(t *testing.T) {
	// 默认 false → 现有行为：整请求切 vision
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(testConfig(), "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.NeedPreprocess || !d.VisionSwitch || d.Upstream != "vision" || d.Model != "claude-3" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_VisionPreprocessNoVision(t *testing.T) {
	// 防御：preprocess 开启但无 vision 上游 → 走 main，不 panic、不标记预处理
	cfg := &config.Config{
		AutoSwitchVision: true,
		VisionPreprocess: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "https://a/v1", APIKey: "k", Format: "openai", Model: "gpt-4o"},
		},
	}
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(cfg, "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.NeedPreprocess || d.VisionSwitch || d.Upstream != "main" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestLatestUserRunHasImage_HistoryImageNotTrigger(t *testing.T) {
	// 历史含图（run 之前），最新 run 纯文本 → false（这是本次语义收窄的核心）
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"这张图显示了一个仪表盘。"},
		{"role":"user","content":"那个数字是多少？"}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("history image must not trigger")
	}
}

func TestLatestUserRunHasImage_RunWithImage(t *testing.T) {
	// 末尾 run 内含图 → true
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":"之前的内容"},
		{"role":"assistant","content":"好的"},
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("run image: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_ConsecutiveUserRun(t *testing.T) {
	// 连续 user 消息 run（首条带图、末条纯文本）→ true：新图不被误判为历史
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"user","content":"看这个"}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("consecutive user run: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_AssistantLast(t *testing.T) {
	// 序列以 assistant 结尾 → run 为其前最后一段连续用户侧消息
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"分析完毕"}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("assistant-last run: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_NoUserMessages(t *testing.T) {
	// 无 user/tool 消息 → false
	body := []byte(`{"model":"x","messages":[{"role":"assistant","content":"hi"}]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("no user messages must be false")
	}
}

func TestLatestUserRunHasImage_OpenAIToolRole(t *testing.T) {
	// OpenAI role=tool 消息含图属于 run 成员（等价 Claude tool_result）
	body := []byte(`{"model":"x","messages":[
		{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"screenshot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("tool role image: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_InvalidBody(t *testing.T) {
	_, err := LatestUserRunHasImage("openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}

const markAnalyzed = "[image: analyzed in previous reply]"
const markOmitted = "[image omitted]"

func TestSanitize_ClaudeImageBlockReplaced(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
		{"role":"assistant","content":[{"type":"text","text":"图中是仪表盘。"}]},
		{"role":"user","content":"数字是多少"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected sanitize")
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)
	first := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != markAnalyzed {
		t.Fatalf("marker: %v", first)
	}
	// assistant 解析文本原位保留
	text := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if text["text"] != "图中是仪表盘。" {
		t.Fatalf("assistant text mutated: %v", text)
	}
	// run 内消息（最后一条）不动
	last := msgs[2].(map[string]any)
	if last["content"] != "数字是多少" {
		t.Fatalf("latest message mutated: %v", last)
	}
}

func TestSanitize_ClaudeNoAssistantText(t *testing.T) {
	// 含图消息后无含文本 assistant 消息 → omitted 标记。
	// 注意：含图消息必须被 assistant 打断（run 边界外），否则两条连续 user 会整体算入 run。
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search","input":{}}]},
		{"role":"user","content":"继续"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	if !strings.Contains(string(out), markOmitted) {
		t.Fatalf("expected omitted marker, got: %s", out)
	}
}

func TestSanitize_ToolLoopFallback(t *testing.T) {
	// 工具循环：截图后紧邻 assistant 是 tool_use（无文本），解析文本在更靠后的 assistant → analyzed
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"search","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"nothing found"}]},
		{"role":"assistant","content":[{"type":"text","text":"截图中是登录页。"}]},
		{"role":"user","content":"继续"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	if !strings.Contains(string(out), markAnalyzed) {
		t.Fatalf("expected analyzed marker (scan across tool loop), got: %s", out)
	}
}

func TestSanitize_OpenAIImageURLReplaced(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"text","text":"看"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"图上有一个按钮"},
		{"role":"user","content":"点哪里"}
	]}`)
	out, changed, err := SanitizeHistoryImages("openai", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	content := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "text" {
		t.Fatalf("parts: %v", content)
	}
	if content[0].(map[string]any)["text"] != "看" {
		t.Fatalf("text part mutated: %v", content[0])
	}
}

func TestSanitize_CacheControlPreserved(t *testing.T) {
	// map 基往返：块级 cache_control 与 tool_result 的 is_error/tool_use_id 保留
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"},"cache_control":{"type":"ephemeral"}}]},
		{"role":"assistant","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":"继续"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	if !strings.Contains(string(out), `"cache_control"`) {
		t.Fatalf("cache_control lost: %s", out)
	}
}

func TestSanitize_NoImageNoop(t *testing.T) {
	// 无图 → no-op：body 逐字节一致，sanitized=false
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no change")
	}
	if string(out) != string(body) {
		t.Fatalf("body not byte-identical: %s", out)
	}
}

func TestSanitize_NoUserRunNoop(t *testing.T) {
	// run 为空（无 user/tool 消息）→ no-op
	body := []byte(`{"model":"x","messages":[{"role":"assistant","content":"hi"}]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || changed {
		t.Fatalf("expected noop: %v %v", changed, err)
	}
	if string(out) != string(body) {
		t.Fatalf("body not byte-identical: %s", out)
	}
}

func TestSanitize_InvalidBody(t *testing.T) {
	_, _, err := SanitizeHistoryImages("openai", []byte("{not json"))
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
