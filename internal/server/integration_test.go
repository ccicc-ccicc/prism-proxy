package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// thinkingCompatReq 是 thinking 模式下的多轮工具调用请求：
// assistant(tool_use) 轮次缺 thinking 块（AIGW/DeepSeek 类上游会拒绝）。
const thinkingCompatReq = `{"model":"x","thinking":{"type":"adaptive"},"messages":[
	{"role":"user","content":"hi"},
	{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}
]}`

// TestThinkingCompat_PatchApplied：thinking_compat=true 时，同格式 claude 透传
// 给缺 thinking 块的 assistant(tool_use) 轮次补空 thinking 块（content[0]），
// 原 tool_use 保留在 content[1]，模型仍改写为配置值。
func TestThinkingCompat_PatchApplied(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req["model"] != "m" {
			t.Fatalf("model not rewritten: %v", req["model"])
		}
		msgs, _ := req["messages"].([]any)
		assistant, ok := msgs[1].(map[string]any)
		if !ok || assistant["role"] != "assistant" {
			t.Fatalf("messages[1] not assistant: %s", body)
		}
		content, _ := assistant["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("assistant content length: %d (want 2): %s", len(content), body)
		}
		first, ok := content[0].(map[string]any)
		if !ok || first["type"] != "thinking" {
			t.Fatalf("assistant(tool_use) missing thinking block: %s", body)
		}
		if th, _ := first["thinking"].(string); th != "" {
			t.Fatalf("thinking block not empty: %s", body)
		}
		second, ok := content[1].(map[string]any)
		if !ok || second["type"] != "tool_use" {
			t.Fatalf("tool_use not preserved: %s", body)
		}
		w.Write([]byte(claudeMockResponse("m", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "m", ThinkingCompat: true},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(thinkingCompatReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, body)
	}
}

// TestThinkingCompat_DisabledPassthrough：thinking_compat 默认 false →
// body 原样透传，assistant(tool_use) 不被补 thinking 块（content 保持单个 tool_use）。
func TestThinkingCompat_DisabledPassthrough(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		msgs, _ := req["messages"].([]any)
		assistant, ok := msgs[1].(map[string]any)
		if !ok || assistant["role"] != "assistant" {
			t.Fatalf("messages[1] not assistant: %s", body)
		}
		content, _ := assistant["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("assistant content modified (want 1 block): %s", body)
		}
		first, ok := content[0].(map[string]any)
		if !ok || first["type"] == "thinking" {
			t.Fatalf("unexpected thinking block: %s", body)
		}
		if first["type"] != "tool_use" {
			t.Fatalf("tool_use not passthrough: %s", body)
		}
		w.Write([]byte(claudeMockResponse("m", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "m"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(thinkingCompatReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, body)
	}
}

// ---------- Vision 预处理（Task 2）----------

// visionPreprocessReq 是 vision 预处理测试入站请求：末尾连续两条 user 消息构成
// 最新 run（"hi" 为 run 内用户文本，第二条含 base64 图）。
const visionPreprocessReq = `{"model":"x","thinking":{"type":"adaptive"},"messages":[
	{"role":"user","content":"hi"},
	{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}
]}`

// newVisionPreprocessProxy 构造 vision 预处理测试代理：AutoSwitchVision 恒开，
// vision/main 独立 mock（独立 BaseURL），按参数指定上游格式与预处理开关。
func newVisionPreprocessProxy(t *testing.T, visionSrv, mainSrv *httptest.Server, visionFormat, mainFormat string, preprocess bool) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		AutoSwitchVision: true,
		VisionPreprocess: preprocess,
		Upstreams: map[string]config.UpstreamConfig{
			"main":   {BaseURL: mainSrv.URL + "/v1", APIKey: "sk", Format: mainFormat, Model: "main-m"},
			"vision": {BaseURL: visionSrv.URL + "/v1", APIKey: "sk", Format: visionFormat, Model: "vision-m"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// postMessages 向代理发送 claude 格式请求并返回响应体。
func postMessages(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// recvBody 等待 mock 收到请求体；5 秒超时防挂起（mock 未被调用时快速失败）。
func recvBody(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("upstream mock not called")
		return ""
	}
}

// assertNoImageBlocks 断言 body 中所有消息 content 顶层块均非 image/image_url
// 且不含 source（即无任何图片块残留）。
func assertNoImageBlocks(t *testing.T, body string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v body: %s", err, body)
	}
	msgs, _ := m["messages"].([]any)
	for _, msg := range msgs {
		mm, _ := msg.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, p := range content {
			pm, _ := p.(map[string]any)
			tpe, _ := pm["type"].(string)
			if tpe == "image" || tpe == "image_url" || pm["source"] != nil {
				t.Fatalf("image block residual: %s", body)
			}
		}
	}
}

// assertVisionRequestShape 断言 vision 预处理请求只含一条 user 消息，
// content 块数恰为 wantParts（图 N 个 + prompt 1 个）。
func assertVisionRequestShape(t *testing.T, body string, wantParts int) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal vision request: %v", err)
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("vision request must have exactly 1 message: %s", body)
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != wantParts {
		t.Fatalf("vision content parts = %d, want %d: %s", len(content), wantParts, body)
	}
}

// TestVisionPreprocess_ImageToMain：预处理模式下最新 run 图片经 vision 解析后替换进请求走 main。
// main 收到无图 body（含 [图片内容: 图中是登录页]）；vision 只收图 + prompt（无历史文本/历史消息）。
func TestVisionPreprocess_ImageToMain(t *testing.T) {
	visionReq := make(chan string, 1)
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visionReq <- string(body)
		w.Write([]byte(`{"choices":[{"message":{"content":"图中是登录页"}}]}`))
	}))
	defer visionSrv.Close()
	mainReq := make(chan string, 1)
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainReq <- string(body)
		w.Write([]byte(claudeMockResponse("main-m", "ok")))
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)

	resp, data := postMessages(t, ts.URL+"/v1/messages", visionPreprocessReq)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}

	mainBody := recvBody(t, mainReq)
	if !strings.Contains(mainBody, "[图片内容: 图中是登录页]") {
		t.Fatalf("main missing parsed text: %s", mainBody)
	}
	assertNoImageBlocks(t, mainBody)

	vBody := recvBody(t, visionReq)
	assertVisionRequestShape(t, vBody, 2)
	if !strings.Contains(vBody, "data:image/png;base64,AAAA") {
		t.Fatalf("vision missing image: %s", vBody)
	}
	if !strings.Contains(vBody, "请依次详细描述每一张图片的内容") {
		t.Fatalf("vision missing fixed prompt: %s", vBody)
	}
	if !strings.Contains(vBody, "结合用户问题「hi」") {
		t.Fatalf("vision missing user context: %s", vBody)
	}
}

// TestVisionPreprocess_UpstreamFail：vision 上游 500 → 客户端 502（错误信封含上游信息）；
// vision 响应不可解析（choices 空）→ 客户端 400（请求侧错误分类）。
func TestVisionPreprocess_UpstreamFail(t *testing.T) {
	t.Run("vision 500 -> 502", func(t *testing.T) {
		visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"message":"vision boom"}}`))
		}))
		defer visionSrv.Close()
		mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("main must not be called on vision failure")
		}))
		defer mainSrv.Close()
		ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)
		resp, data := postMessages(t, ts.URL+"/v1/messages", visionPreprocessReq)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status: %d (want 502) body: %s", resp.StatusCode, data)
		}
		if !strings.Contains(string(data), "vision") || !strings.Contains(string(data), "500") {
			t.Fatalf("error envelope missing upstream info: %s", data)
		}
	})
	t.Run("vision empty choices -> 400", func(t *testing.T) {
		visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Write([]byte(`{"choices":[]}`))
		}))
		defer visionSrv.Close()
		mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("main must not be called")
		}))
		defer mainSrv.Close()
		ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)
		resp, data := postMessages(t, ts.URL+"/v1/messages", visionPreprocessReq)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: %d (want 400) body: %s", resp.StatusCode, data)
		}
	})
}

// TestVisionPreprocess_ClaudeVisionUpstream：vision 上游为 claude 格式 → 请求含
// max_tokens（必设）与 image block；响应 content 块解析文本回填 main。
func TestVisionPreprocess_ClaudeVisionUpstream(t *testing.T) {
	visionReq := make(chan string, 1)
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visionReq <- string(body)
		w.Write([]byte(`{"content":[{"type":"text","text":"图中是仪表盘"}],"role":"assistant","type":"message"}`))
	}))
	defer visionSrv.Close()
	mainReq := make(chan string, 1)
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainReq <- string(body)
		w.Write([]byte(claudeMockResponse("main-m", "ok")))
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "claude", "claude", true)

	resp, data := postMessages(t, ts.URL+"/v1/messages", visionPreprocessReq)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}

	vBody := recvBody(t, visionReq)
	assertVisionRequestShape(t, vBody, 2)
	if !strings.Contains(vBody, `"max_tokens":1024`) {
		t.Fatalf("claude vision request missing max_tokens: %s", vBody)
	}
	if !strings.Contains(vBody, `"media_type":"image/png"`) || !strings.Contains(vBody, `"data":"AAAA"`) {
		t.Fatalf("claude vision request missing image source: %s", vBody)
	}
	mainBody := recvBody(t, mainReq)
	if !strings.Contains(mainBody, "[图片内容: 图中是仪表盘]") {
		t.Fatalf("main missing parsed text: %s", mainBody)
	}
	assertNoImageBlocks(t, mainBody)
}

// TestVisionPreprocess_ContentArray：OpenAI 响应 content 为数组 → 拼接全部 text part。
func TestVisionPreprocess_ContentArray(t *testing.T) {
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}}]}`))
	}))
	defer visionSrv.Close()
	mainReq := make(chan string, 1)
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainReq <- string(body)
		w.Write([]byte(claudeMockResponse("main-m", "ok")))
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)

	resp, data := postMessages(t, ts.URL+"/v1/messages", visionPreprocessReq)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}
	mainBody := recvBody(t, mainReq)
	if !strings.Contains(mainBody, "[图片内容: AB]") {
		t.Fatalf("content array not joined: %s", mainBody)
	}
}

// TestVisionPreprocess_StreamPassthrough：入站 stream=true → 预处理后 main 流式透传
// （stream 字段保留），客户端收到 SSE 结束帧。
func TestVisionPreprocess_StreamPassthrough(t *testing.T) {
	streamBody := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[]}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	visionReq := make(chan string, 1)
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visionReq <- string(body)
		w.Write([]byte(`{"choices":[{"message":{"content":"图中是登录页"}}]}`))
	}))
	defer visionSrv.Close()
	mainReq := make(chan string, 1)
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainReq <- string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		w.Write([]byte(streamBody))
		fl.Flush()
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)

	req := `{"model":"x","messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}],"stream":true}`
	resp, data := postMessages(t, ts.URL+"/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "message_stop") {
		t.Fatalf("stream end frame missing: %s", data)
	}
	if !strings.Contains(string(data), `"text":"hi"`) {
		t.Fatalf("stream delta missing: %s", data)
	}
	// 预处理照常发生（vision 收到图 + prompt），main 收到 stream 透传 + 解析文本
	recvBody(t, visionReq)
	mainBody := recvBody(t, mainReq)
	var m map[string]any
	_ = json.Unmarshal([]byte(mainBody), &m)
	if st, _ := m["stream"].(bool); !st {
		t.Fatalf("stream field not preserved: %s", mainBody)
	}
	if !strings.Contains(mainBody, "[图片内容: 图中是登录页]") {
		t.Fatalf("preprocess not applied on stream: %s", mainBody)
	}
}

// TestVisionPreprocess_NoUserText：run 只有图无用户文本 → vision 只收固定指令（无"结合用户问题"）。
func TestVisionPreprocess_NoUserText(t *testing.T) {
	visionReq := make(chan string, 1)
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visionReq <- string(body)
		w.Write([]byte(`{"choices":[{"message":{"content":"纯图描述"}}]}`))
	}))
	defer visionSrv.Close()
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(claudeMockResponse("main-m", "ok")))
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)

	req := `{"model":"x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	resp, data := postMessages(t, ts.URL+"/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}
	vBody := recvBody(t, visionReq)
	if !strings.Contains(vBody, "请依次详细描述每一张图片的内容") {
		t.Fatalf("missing fixed prompt: %s", vBody)
	}
	if strings.Contains(vBody, "结合用户问题") {
		t.Fatalf("user context should be absent: %s", vBody)
	}
}

// TestVisionPreprocess_Disabled：VisionPreprocess=false → 现有行为：整请求切 vision
// （vision 收到含历史文本的完整请求）。
func TestVisionPreprocess_Disabled(t *testing.T) {
	visionReq := make(chan string, 1)
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visionReq <- string(body)
		w.Write([]byte(claudeMockResponse("vision-m", "ok")))
	}))
	defer visionSrv.Close()
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("main must not be called")
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "claude", "claude", false)

	req := `{"model":"x","messages":[
		{"role":"user","content":"历史问题"},
		{"role":"assistant","content":"历史回答"},
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}
	]}`
	resp, data := postMessages(t, ts.URL+"/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}
	vBody := recvBody(t, visionReq)
	if !strings.Contains(vBody, "历史问题") || !strings.Contains(vBody, "历史回答") {
		t.Fatalf("vision must receive full request with history: %s", vBody)
	}
	if !strings.Contains(vBody, `"data":"AAAA"`) {
		t.Fatalf("vision must receive the image: %s", vBody)
	}
}

// TestVisionPreprocess_HistorySanitized：历史含图 + 最新 run 含图 → 历史图脱敏标记 +
// 最新图解析文本同时进入 main（预处理与历史脱敏共存）。
func TestVisionPreprocess_HistorySanitized(t *testing.T) {
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "HIST") {
			t.Fatalf("history image leaked into vision request: %s", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"最新图解析"}}]}`))
	}))
	defer visionSrv.Close()
	mainReq := make(chan string, 1)
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainReq <- string(body)
		w.Write([]byte(claudeMockResponse("main-m", "ok")))
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)

	req := `{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"HIST"}}]},
		{"role":"assistant","content":"历史图解析"},
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}
	]}`
	resp, data := postMessages(t, ts.URL+"/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}
	mainBody := recvBody(t, mainReq)
	if !strings.Contains(mainBody, "[image: analyzed in previous reply]") {
		t.Fatalf("history image not sanitized: %s", mainBody)
	}
	if !strings.Contains(mainBody, "[图片内容: 最新图解析]") {
		t.Fatalf("latest parsed text missing: %s", mainBody)
	}
	if strings.Contains(mainBody, "HIST") || strings.Contains(mainBody, "AAAA") {
		t.Fatalf("raw image data leaked to main: %s", mainBody)
	}
}

// TestVisionPreprocess_URLOmittedSkipsVision：run 内 URL 图（非 base64）→ 不入 imgs、
// 降级 [image omitted]，跳过 vision 调用直接走 main。
func TestVisionPreprocess_URLOmittedSkipsVision(t *testing.T) {
	visionCalled := make(chan struct{}, 1)
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionCalled <- struct{}{}
		t.Fatalf("vision must not be called for URL-only images")
	}))
	defer visionSrv.Close()
	mainReq := make(chan string, 1)
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainReq <- string(body)
		w.Write([]byte(claudeMockResponse("main-m", "ok")))
	}))
	defer mainSrv.Close()
	ts := newVisionPreprocessProxy(t, visionSrv, mainSrv, "openai", "claude", true)

	req := `{"model":"x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]}]}`
	resp, data := postMessages(t, ts.URL+"/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, data)
	}
	mainBody := recvBody(t, mainReq)
	if !strings.Contains(mainBody, "[image omitted]") {
		t.Fatalf("URL image not downgraded: %s", mainBody)
	}
	select {
	case <-visionCalled:
		t.Fatal("vision was called for URL-only images")
	default:
	}
}
