package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-proxy/internal/config"
	"prism-proxy/internal/upstream"
)

// mockUpstream 按 format 提供最小可用响应。
type mockUpstream struct {
	format  string
	model   string
	handler http.HandlerFunc
}

func openaiMockResponse(model string, content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "model": model,
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
	return string(b)
}

func claudeMockResponse(model string, content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]any{{"type": "text", "text": content}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
	})
	return string(b)
}

func TestMatrix_AllQuadrants(t *testing.T) {
	quadrants := []struct {
		inboundFormat  string
		upstreamFormat string
		path           string
		reqBody        string
	}{
		{"openai", "openai", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"openai", "claude", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"claude", "openai", "/v1/messages", `{"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`},
		{"claude", "claude", "/v1/messages", `{"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, q := range quadrants {
		t.Run(q.inboundFormat+"->"+q.upstreamFormat, func(t *testing.T) {
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if q.upstreamFormat == "openai" {
					w.Write([]byte(openaiMockResponse("gpt-4o", "answer")))
				} else {
					w.Write([]byte(claudeMockResponse("claude-3", "answer")))
				}
			}))
			defer upstreamSrv.Close()
			cfg := &config.Config{
				Upstreams: map[string]config.UpstreamConfig{
					"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: q.upstreamFormat, Model: "m"},
				},
			}
			srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
			ts := httptest.NewServer(srv)
			defer ts.Close()
			resp, err := http.Post(ts.URL+q.path, "application/json", strings.NewReader(q.reqBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status: %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "answer") {
				t.Fatalf("body: %s", body)
			}
			// 客户端格式断言
			if q.inboundFormat == "openai" {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil || m["choices"] == nil {
					t.Fatalf("openai shape: %v %s", err, body)
				}
			} else {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil || m["content"] == nil {
					t.Fatalf("claude shape: %v %s", err, body)
				}
			}
		})
	}
}

func TestMatrix_StreamingAllQuadrants(t *testing.T) {
	// 流式：每个象限上游返回对应格式 SSE，断言客户端收到自己格式的流。
	// 断言子串对齐状态机实际输出：
	//   - C2O chunk 的 delta 按 struct 字段序为 {"role":...,"content":...}，
	//     故 openai 客户端断言 "content":"hi"（顺序稳定的子串）；
	//   - O2C 帧的 data 行 type 字段等于事件名（如 message_delta），
	//     故 claude 客户端断言 event 行 + delta 内容子串。
	quadrants := []struct {
		inboundFormat, upstreamFormat, path, reqBody, upStream, clientChunk, clientChunkExtra string
	}{
		{"openai", "openai", "/v1/chat/completions", `{"messages":[],"stream":true}`,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			`"delta":{"content":"hi"}`, ""},
		{"openai", "claude", "/v1/chat/completions", `{"messages":[],"stream":true}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[]}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			`"content":"hi"`, ""},
		{"claude", "openai", "/v1/messages", `{"max_tokens":1024,"messages":[],"stream":true}`,
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			"event: content_block_delta", `"text":"hi"`},
		{"claude", "claude", "/v1/messages", `{"max_tokens":1024,"messages":[],"stream":true}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[]}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			`"text":"hi"`, ""},
	}
	for _, q := range quadrants {
		t.Run(q.inboundFormat+"->"+q.upstreamFormat+" stream", func(t *testing.T) {
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				fl, _ := w.(http.Flusher)
				w.Write([]byte(q.upStream))
				fl.Flush()
			}))
			defer upstreamSrv.Close()
			cfg := &config.Config{
				Upstreams: map[string]config.UpstreamConfig{
					"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: q.upstreamFormat, Model: "m"},
				},
			}
			srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
			ts := httptest.NewServer(srv)
			defer ts.Close()
			resp, err := http.Post(ts.URL+q.path, "application/json", strings.NewReader(q.reqBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), q.clientChunk) {
				t.Fatalf("client stream: %s", body)
			}
			if q.clientChunkExtra != "" && !strings.Contains(string(body), q.clientChunkExtra) {
				t.Fatalf("client stream extra: %s", body)
			}
			if q.inboundFormat == "openai" && !strings.Contains(string(body), "[DONE]") {
				t.Fatalf("missing DONE: %s", body)
			}
		})
	}
}

func TestMatrix_Tools_ToolCallsQuadrants(t *testing.T) {
	// 工具调用：OpenAI 入站带 tools + 上游返回 tool_use（Claude）或 tool_calls（OpenAI）
	// 覆盖 O2C 工具转换 + C2O 工具转换
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"tokyo"}}],"stop_reason":"tool_use"}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req := `{"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"tool_calls"`) || !strings.Contains(string(body), "get_weather") {
		t.Fatalf("tool_calls: %s", body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	tc := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["function"].(map[string]any)["arguments"] != `{"city":"tokyo"}` {
		t.Fatalf("arguments: %v", tc["function"])
	}
}

func TestMatrix_ImageRouting(t *testing.T) {
	// 带图请求四象限都路由到 vision（若配置）+ 文本零行为变化
	// 主要场景已由 TestVisionSwitch_ToClaudeVision 覆盖；此处补：vision 未配置 → main 收到带图请求
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "image_url") {
			t.Fatalf("image not forwarded: %s", body)
		}
		w.Write([]byte(openaiMockResponse("gpt-4o", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{ // auto_switch_vision 缺省 false
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

// TestMatrix_Non2xxCrossFormatPassthrough 交叉格式错误透传：
// claude 上游 429 错误体 → openai 客户端收到 429 且 body 逐字节原样（绝不转换）。
func TestMatrix_Non2xxCrossFormatPassthrough(t *testing.T) {
	claudeErr := `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(claudeErr))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != claudeErr {
		t.Fatalf("error body not verbatim: %q", body)
	}
}

// TestMatrix_OutboundStreamFlag 交叉格式出站 body 的 stream 字段必须与请求一致。
func TestMatrix_OutboundStreamFlag(t *testing.T) {
	cases := []struct {
		name       string
		upFormat   string
		path       string
		reqBody    string
		wantStream bool
	}{
		{"o2c stream=true", "claude", "/v1/chat/completions", `{"messages":[],"stream":true}`, true},
		{"o2c stream=false", "claude", "/v1/chat/completions", `{"messages":[]}`, false},
		{"c2o stream=true", "openai", "/v1/messages", `{"max_tokens":1024,"messages":[],"stream":true}`, true},
		{"c2o stream=false", "openai", "/v1/messages", `{"max_tokens":1024,"messages":[]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil {
					t.Fatalf("outbound parse: %v", err)
				}
				stream, _ := m["stream"].(bool)
				if stream != c.wantStream {
					t.Fatalf("outbound stream=%v, want %v (body %s)", stream, c.wantStream, body)
				}
				if c.upFormat == "openai" {
					w.Write([]byte(openaiMockResponse("gpt-4o", "ok")))
				} else {
					w.Write([]byte(claudeMockResponse("claude-3", "ok")))
				}
			}))
			defer upstreamSrv.Close()
			cfg := &config.Config{
				Upstreams: map[string]config.UpstreamConfig{
					"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: c.upFormat, Model: "m"},
				},
			}
			srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
			ts := httptest.NewServer(srv)
			defer ts.Close()
			resp, err := http.Post(ts.URL+c.path, "application/json", strings.NewReader(c.reqBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status: %d", resp.StatusCode)
			}
		})
	}
}

// TestSanitize_HistoryImageToMain：auto_switch=true + 历史含图 + 最新纯文本 →
// main 收到无图 body（历史图替换为标记），且 model 改写与脱敏共存。
func TestSanitize_HistoryImageToMain(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "image_url") || strings.Contains(string(body), `"image"`) {
			t.Fatalf("image not sanitized: %s", body)
		}
		if !strings.Contains(string(body), "[image: analyzed") {
			t.Fatalf("marker missing: %s", body)
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if m["model"] != "m" {
			t.Fatalf("model not rewritten: %v", m["model"])
		}
		w.Write([]byte(openaiMockResponse("m", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "m"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req := `{"model":"ignored","messages":[
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"图上有个按钮"},
		{"role":"user","content":"点哪里"}
	]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, body)
	}
}

// TestSanitize_NewImageGoesVision：最新 run 含图 → vision 收到原样 body（不脱敏）。
func TestSanitize_NewImageGoesVision(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"type":"image"`) || !strings.Contains(string(body), "source") {
			// 注：vision 上游为 claude 格式，O2C 转换后 image_url 块变为
			// {"type":"image","source":{...}}——按测试意图断言图片内容存续。
			t.Fatalf("image lost on vision relay: %s", body)
		}
		w.Write([]byte(claudeMockResponse("claude-3", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main":   {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req := `{"model":"ignored","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, body)
	}
}
