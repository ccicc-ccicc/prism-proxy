package route

import (
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
