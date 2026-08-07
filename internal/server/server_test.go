package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"prism-proxy/internal/config"
	"prism-proxy/internal/trafficlog"
	"prism-proxy/internal/upstream"
)

// failingWriter 模拟下游断开：Write 必然失败。
type failingWriter struct {
	h http.Header
}

func (f *failingWriter) Header() http.Header { return f.h }
func (f *failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("write: client disconnected")
}
func (f *failingWriter) WriteHeader(int) {}
func (f *failingWriter) Flush()          {}

func TestRewriteModel(t *testing.T) {
	out, err := rewriteModel("openai", []byte(`{"model":"old","messages":[]}`), "new-model")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"model":"new-model"`) || strings.Contains(string(out), "old") {
		t.Fatalf("rewrite: %s", out)
	}
}

func TestPassthrough_OpenAIToOpenAI(t *testing.T) {
	upstreamHit := false
	var gotModel string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		gotModel, _ = m["model"].(string)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"` + gotModel + `","choices":[]}`))
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
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"anything-client-sent","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !upstreamHit {
		t.Fatalf("status=%d hit=%v", resp.StatusCode, upstreamHit)
	}
	if gotModel != "gpt-4o" {
		t.Fatalf("upstream saw model %q, want gpt-4o", gotModel)
	}
}

func TestAuth_RejectAndAccept(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{AuthKeys: []string{"sk-proxy-1"}},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("no auth: %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-proxy-1")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("with auth: %d", resp2.StatusCode)
	}
}

func TestErrorPassthrough(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
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
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "rate limited") {
		t.Fatalf("body: %s", b)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		fl.Flush()
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		fl.Flush()
		w.Write([]byte("data: [DONE]\n\n"))
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
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: %s", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "hi") || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("stream body: %s", b)
	}
}

func TestNotFound(t *testing.T) {
	srv := NewWithConfig(&config.Config{Upstreams: map[string]config.UpstreamConfig{
		"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
	}}, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/unknown", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown path: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET on post-only path: %d", resp.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	srv := NewWithConfig(&config.Config{Upstreams: map[string]config.UpstreamConfig{
		"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
	}}, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	big := strings.Repeat("x", maxBody+1)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}
}

func TestInvalidJSON(t *testing.T) {
	srv := NewWithConfig(&config.Config{Upstreams: map[string]config.UpstreamConfig{
		"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
	}}, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid json: %d", resp.StatusCode)
	}
}

// Important 4: 401/400/413 错误必须以入站协议信封返回（Content-Type: application/json）。
func TestErrorEnvelope_OpenAI401(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{AuthKeys: []string{"sk-proxy-1"}},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("openai envelope: %v (%s)", err, body)
	}
	if out.Error.Message == "" || out.Error.Type != "authentication_error" {
		t.Fatalf("envelope: %+v", out)
	}
}

func TestErrorEnvelope_Claude401(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{AuthKeys: []string{"sk-proxy-1"}},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("claude envelope: %v (%s)", err, body)
	}
	if out.Type != "error" || out.Error.Type != "authentication_error" || out.Error.Message == "" {
		t.Fatalf("envelope: %+v", out)
	}
}

func TestErrorEnvelope_OpenAI400(t *testing.T) {
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			// claude 格式上游：触发 O2C 请求转换路径，n>1 在 OpenAIRequestToClaude 被拒绝
			"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// n>1 拒绝 → 400（OpenAIRequestToClaude 错误经 400 路径）
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[],"n":2}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("envelope: %v (%s)", err, body)
	}
	if out.Error.Message == "" || out.Error.Type != "invalid_request_error" {
		t.Fatalf("envelope: %+v", out)
	}
}

func TestErrorEnvelope_Claude400(t *testing.T) {
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("claude envelope: %v (%s)", err, body)
	}
	if out.Type != "error" || out.Error.Type != "invalid_request_error" || out.Error.Message == "" {
		t.Fatalf("envelope: %+v", out)
	}
}

func TestErrorEnvelope_413(t *testing.T) {
	srv := NewWithConfig(&config.Config{Upstreams: map[string]config.UpstreamConfig{
		"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
	}}, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	big := strings.Repeat("x", maxBody+1)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil || out.Error.Message == "" {
		t.Fatalf("envelope: %v (%s)", err, body)
	}
}

// Minor 3: spec §3 两个头都查——Authorization: Bearer 与 x-api-key 任一命中即过，
// 与入口路径无关（跨头携带 key 也放行）。
func TestAuth_CrossHeader(t *testing.T) {
	run := func(path, upFormat string) {
		upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		}))
		defer upstreamSrv.Close()
		cfg := &config.Config{
			Server: config.ServerConfig{AuthKeys: []string{"sk-proxy-1"}},
			Upstreams: map[string]config.UpstreamConfig{
				"main": {BaseURL: upstreamSrv.URL, APIKey: "sk", Format: upFormat, Model: "m"},
			},
		}
		srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
		ts := httptest.NewServer(srv)
		defer ts.Close()

		// 每个路径 × 每个头（含跨头：非规范头携带 key）都必须放行
		for _, header := range []string{"Authorization: Bearer sk-proxy-1", "x-api-key: sk-proxy-1"} {
			req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(`{"messages":[]}`))
			parts := strings.SplitN(header, ": ", 2)
			req.Header.Set(parts[0], parts[1])
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s on %s: %d, want 200", header, path, resp.StatusCode)
			}
		}
		// 错误 key（任一头的值不匹配）→ 401
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(`{"messages":[]}`))
		req.Header.Set("x-api-key", "wrong-key")
		req.Header.Set("Authorization", "Bearer also-wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("wrong keys on %s: %d, want 401", path, resp.StatusCode)
		}
	}
	run("/v1/chat/completions", "openai")
	run("/v1/messages", "claude")
}

func TestUpstreamError(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	dead := upstreamSrv.URL
	upstreamSrv.Close() // 连接被拒绝 → client.Do 报错
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: dead + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down: %d", resp.StatusCode)
	}
}

// Minor 9: 502 错误体不得回显上游 URL（完整错误含 URL，保留在服务端日志）。
func TestUpstreamError_NoURLInClientBody(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := upstreamSrv.URL
	upstreamSrv.Close() // 连接被拒绝 → client.Do 报错（错误串含上游 URL）
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: dead + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	u, perr := url.Parse(dead)
	if perr != nil {
		t.Fatal(perr)
	}
	if strings.Contains(string(body), u.Host) {
		t.Fatalf("client-visible 502 body leaks upstream host %q: %s", u.Host, body)
	}
}

func TestUpstreamMissing(t *testing.T) {
	srv := NewWithConfig(&config.Config{Upstreams: map[string]config.UpstreamConfig{}}, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("missing upstream: %d", resp.StatusCode)
	}
}

func TestCopyStreamWriteError(t *testing.T) {
	srv := NewWithConfig(&config.Config{}, upstream.NewClient(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fw := &failingWriter{h: http.Header{}}
	err := srv.copyStream(fw, req, strings.NewReader("data: x\n\n"))
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("want write error, got %v", err)
	}
}

func TestCopyStreamClientCancel(t *testing.T) {
	srv := NewWithConfig(&config.Config{}, upstream.NewClient(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	pr, pw := io.Pipe()
	defer pw.Close() // 解除可能残留的读阻塞
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	err := srv.copyStream(&failingWriter{h: http.Header{}}, req, pr)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestTrafficLog_NonStreamingFullCapture(t *testing.T) {
	var buf bytes.Buffer
	tl := trafficlog.NewWithWriter(&buf)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"up-1","object":"chat.completion","model":"gpt-4o","choices":[]}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Logging: config.LoggingConfig{Enabled: true, Dir: t.TempDir(), MaxFiles: 3},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	srv.SetTrafficLog(tl)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id header")
	}
	line := strings.TrimSpace(buf.String())
	var e map[string]any
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("parse entry: %v", err)
	}
	if e["request_id"] != resp.Header.Get("X-Request-Id") {
		t.Fatalf("request_id mismatch: %v vs %v", e["request_id"], resp.Header.Get("X-Request-Id"))
	}
	if !strings.Contains(e["inbound_body"].(string), "hi") {
		t.Fatalf("inbound_body: %v", e["inbound_body"])
	}
	if !strings.Contains(e["outbound_body"].(string), "gpt-4o") {
		t.Fatalf("outbound_body: %v", e["outbound_body"])
	}
	if !strings.Contains(e["upstream_response"].(string), "up-1") {
		t.Fatalf("upstream_response: %v", e["upstream_response"])
	}
	if e["outbound_response"].(string) != e["upstream_response"].(string) {
		t.Fatalf("passthrough outbound should equal upstream: %v vs %v", e["outbound_response"], e["upstream_response"])
	}
	if e["status"] != float64(200) {
		t.Fatalf("status: %v", e["status"])
	}
}

func TestTrafficLog_DisabledNoCapture(t *testing.T) {
	var buf bytes.Buffer
	tl := trafficlog.NewWithWriter(&buf)
	cfg := &config.Config{
		Logging: config.LoggingConfig{Enabled: false, Dir: t.TempDir(), MaxFiles: 3},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "http://127.0.0.1:1", APIKey: "sk", Format: "openai", Model: "m"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	srv.SetTrafficLog(tl)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if buf.Len() != 0 {
		t.Fatalf("disabled should not log, got: %s", buf.String())
	}
}

func TestTrafficLog_UpstreamErrorPassthroughRecorded(t *testing.T) {
	var buf bytes.Buffer
	tl := trafficlog.NewWithWriter(&buf)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Logging: config.LoggingConfig{Enabled: true, Dir: t.TempDir(), MaxFiles: 3},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "m"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	srv.SetTrafficLog(tl)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var e map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &e); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e["status"] != float64(429) {
		t.Fatalf("status: %v", e["status"])
	}
	if !strings.Contains(e["error"].(string), "upstream status 429") {
		t.Fatalf("error: %v", e["error"])
	}
	if !strings.Contains(e["upstream_response"].(string), "rate limited") {
		t.Fatalf("upstream_response: %v", e["upstream_response"])
	}
}
