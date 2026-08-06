package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
	"prism-proxy/internal/upstream"
)

// captureHandler 记录 slog 输出，供断言 proxy_request 日志行。
type captureHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" " + a.Key + "=" + fmt.Sprint(a.Value.Any()))
		return true
	})
	h.lines = append(h.lines, sb.String())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func TestO2CFullChain_NonStream(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 断言收到的是 Claude 格式请求
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("claude body: %v", err)
		}
		if _, hasSystem := req["system"]; !hasSystem {
			t.Fatalf("claude request missing system: %s", body)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"hello from claude"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
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
	// 客户端以 OpenAI 格式发送
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("client should get openai format: %v", err)
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello from claude" {
		t.Fatalf("content: %v", msg["content"])
	}
}

func TestC2OFullChain_NonStream(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi from gpt"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// 客户端以 Claude 格式发送
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("client should get claude format: %v", err)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "hi from gpt" {
		t.Fatalf("content: %v", content)
	}
}

func TestO2CStream_ThroughServer(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			fl.Flush()
		}
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
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"delta":{"content":"hi"}`) && !strings.Contains(s, "hi") {
		t.Fatalf("stream body: %s", s)
	}
	if !strings.Contains(s, "[DONE]") {
		t.Fatalf("missing DONE: %s", s)
	}
}

func TestVisionSwitch_ToClaudeVision(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"claude-3-5-vision"`) {
			t.Fatalf("vision model not rewritten: %s", body)
		}
		if !strings.Contains(string(body), `"type":"image"`) {
			t.Fatalf("image block missing: %s", body)
		}
		if !strings.Contains(string(body), `"media_type":"image/png"`) {
			t.Fatalf("media_type missing: %s", body)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_v","type":"message","role":"assistant","model":"claude-3-5-vision","content":[{"type":"text","text":"i see the image"}],"stop_reason":"end_turn"}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main":   {BaseURL: "http://unused/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk-v", Format: "claude", Model: "claude-3-5-vision"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// OpenAI 客户端带图 → 自动切 vision（Claude 格式上游）
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "i see the image") {
		t.Fatalf("body: %s", b)
	}
}

func TestFetchExternalImages_BlockShape(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake-image-bytes"))
	}))
	defer imgSrv.Close()
	req := &convert.MessagesRequest{
		Messages: []convert.ClaudeMessage{
			{Role: "user", Content: []convert.ClaudeBlock{
				{Type: "image", Source: &convert.ImageSource{Type: "url", Data: imgSrv.URL + "/img.png"}},
			}},
		},
	}
	if err := fetchExternalImages(req); err != nil {
		t.Fatal(err)
	}
	blocks := req.Messages[0].Content.([]convert.ClaudeBlock)
	src := blocks[0].Source
	want := base64.StdEncoding.EncodeToString([]byte("fake-image-bytes"))
	if src.Type != "base64" || src.MediaType != "image/png" || src.Data != want {
		t.Fatalf("source: %+v", src)
	}
	// 非 http(s) scheme 必须拒绝
	bad := &convert.MessagesRequest{
		Messages: []convert.ClaudeMessage{
			{Role: "user", Content: []convert.ClaudeBlock{
				{Type: "image", Source: &convert.ImageSource{Type: "url", Data: "file:///etc/passwd"}},
			}},
		},
	}
	if err := fetchExternalImages(bad); err == nil {
		t.Fatal("want scheme error")
	}
}

func TestFetchExternalImages_MapShape(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpeg-bytes"))
	}))
	defer imgSrv.Close()
	req := &convert.MessagesRequest{
		Messages: []convert.ClaudeMessage{
			{Role: "user", Content: []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "url", "data": imgSrv.URL + "/a.jpg"}},
			}},
		},
	}
	if err := fetchExternalImages(req); err != nil {
		t.Fatal(err)
	}
	parts := req.Messages[0].Content.([]any)
	src := parts[0].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != base64.StdEncoding.EncodeToString([]byte("jpeg-bytes")) {
		t.Fatalf("source: %v", src)
	}
}

// 上游中途发非法 chunk（O2C 转换失败）→ 不得伪造 message_stop 结束帧。
func TestO2CStream_MidStreamError_NoMessageStop(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		fl.Flush()
		w.Write([]byte("data: not-json\n\n")) // 非法 chunk → 转换失败
		fl.Flush()
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	// 错误前已转换的内容必须回写
	if !strings.Contains(s, "content_block_delta") {
		t.Fatalf("missing converted frames: %s", s)
	}
	// 中途错误 → 不得伪造 message_stop
	if strings.Contains(s, "message_stop") {
		t.Fatalf("fabricated message_stop on mid-stream error: %s", s)
	}
}

// 交叉格式流式成功也必须产生 proxy_request 日志行。
func TestO2CStream_SuccessLogged(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			fl.Flush()
		}
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	cap := &captureHandler{}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.New(cap))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, l := range cap.lines {
		if strings.Contains(l, "proxy_request") && strings.Contains(l, "status=200") && strings.Contains(l, "stream=true") {
			return
		}
	}
	t.Fatalf("no success log line for cross-format stream: %v", cap.lines)
}

// 挂起的读必须被空闲超时打断（timeoutReader 复用 readWithCtx 模式）。
func TestTimeoutReader_StalledUpstream(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close() // 解除读协程阻塞
	rd := newTimeoutReader(context.Background(), pr, 30*time.Millisecond)
	buf := make([]byte, 16)
	_, err := rd.Read(buf)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

// Critical 1: C2O 流中途上游 error 事件 → 已回写内容保留，但客户端拿不到 [DONE]，
// proxy_request 日志记录 error（不把截断流伪装成成功）。
func TestC2OStream_ErrorEvent_NoDONE(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
	}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			fl.Flush()
		}
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	cap := &captureHandler{}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.New(cap))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// 错误前已转换的内容必须回写
	if !strings.Contains(string(body), "hi") {
		t.Fatalf("missing converted content: %s", body)
	}
	// 上游错误 → 绝不伪造 [DONE]
	if strings.Contains(string(body), "[DONE]") {
		t.Fatalf("fabricated [DONE] on upstream stream error: %s", body)
	}
	// 日志必须记录上游流错误
	cap.mu.Lock()
	defer cap.mu.Unlock()
	found := false
	for _, l := range cap.lines {
		if strings.Contains(l, "proxy_request") && strings.Contains(l, "upstream stream error") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error log line: %v", cap.lines)
	}
}

// Critical 2: C2O 全链路带图请求（Claude 客户端 → OpenAI 上游）：
// 上游请求体必须包含 image_url part 与 data URL（spec §5）。
func TestC2OFullChain_Image(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"image_url"`) {
			t.Fatalf("image_url missing in upstream request: %s", body)
		}
		if !strings.Contains(string(body), "data:image/png;base64,iVBORw0KGgo=") {
			t.Fatalf("data url missing in upstream request: %s", body)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("openai request parse: %v", err)
		}
		msgs := req["messages"].([]any)
		parts := msgs[0].(map[string]any)["content"].([]any)
		if len(parts) != 2 || parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["type"] != "image_url" {
			t.Fatalf("content parts: %v", parts)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"what"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d (%s)", resp.StatusCode, body)
	}
}

// 交叉格式非流式上游响应超限 → 干净 413（而非解析失败 400）。
func TestO2CResponseTooLarge(t *testing.T) {
	big := strings.Repeat("x", maxBody+1)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(big))
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
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("status: %d, want 413", resp.StatusCode)
	}
}

// Minor 7: 上游返回无法解析的响应体（2xx）→ 502（上游来源错误，非 400）。
func TestUpstreamResponseParseError_502(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`not json`))
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
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status: %d, want 502", resp.StatusCode)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil || out.Error.Message == "" {
		t.Fatalf("502 must use inbound error envelope: %v (%s)", err, body)
	}
}

// Minor 11: 检测到图片但未切换（开关关）→ proxy_request 日志含 image_detected=true；
// 文本请求无该字段。
func TestImageDetectedLog_NotSwitched(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(openaiMockResponse("gpt-4o", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{ // auto_switch_vision 缺省 false
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	cap := &captureHandler{}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.New(cap))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// 带图请求 → image_detected=true
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// 文本请求 → 无 image_detected
	resp2, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	cap.mu.Lock()
	defer cap.mu.Unlock()
	imageLogged, textLogged := false, false
	for _, l := range cap.lines {
		if !strings.Contains(l, "proxy_request") {
			continue
		}
		if strings.Contains(l, "image_detected=true") {
			imageLogged = true
		} else {
			textLogged = true
		}
	}
	if !imageLogged || !textLogged {
		t.Fatalf("expected one image_detected=true and one plain line: %v", cap.lines)
	}
}

// Minor 11: 切换命中（vision_switch=true）时不再记 image_detected。
func TestImageDetectedLog_Switched(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(claudeMockResponse("claude-3-5-vision", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main":   {BaseURL: "http://unused/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk-v", Format: "claude", Model: "claude-3-5-vision"},
		},
	}
	cap := &captureHandler{}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.New(cap))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, l := range cap.lines {
		if !strings.Contains(l, "proxy_request") {
			continue
		}
		if strings.Contains(l, "vision_switch=true") {
			if strings.Contains(l, "image_detected=true") {
				t.Fatalf("switched request must not log image_detected: %s", l)
			}
			return
		}
	}
	t.Fatalf("no vision_switch=true log line: %v", cap.lines)
}

// Important 5: 外链图片下载最多跟随 3 次重定向，超出报错。
func TestDownloadImage_RedirectCap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/a", http.StatusFound) })
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusFound) })
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/img", http.StatusFound) })
	mux.HandleFunc("/img", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("img"))
	})
	imgSrv := httptest.NewServer(mux)
	defer imgSrv.Close()
	// /start → /a → /b → /c → /img：4 次重定向，超过上限 3 → 报错
	if _, err := downloadImage(imgSrv.URL + "/start"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect cap must fail with redirect error, got %v", err)
	}
	// 3 次以内重定向应成功
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/img", http.StatusFound) })
	mux2.HandleFunc("/img", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("img"))
	})
	imgSrv2 := httptest.NewServer(mux2)
	defer imgSrv2.Close()
	src, err := downloadImage(imgSrv2.URL + "/a")
	if err != nil || src.MediaType != "image/png" {
		t.Fatalf("1 redirect should succeed: %+v %v", src, err)
	}
}
