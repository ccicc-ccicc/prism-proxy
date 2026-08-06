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

func TestDecide_InvalidBody(t *testing.T) {
	_, err := Decide(testConfig(), "openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}
