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

// 交叉格式非流式上游响应超限 → 干净 400（而非解析失败）。
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
	if resp.StatusCode != 400 {
		t.Fatalf("status: %d, want 400", resp.StatusCode)
	}
}
