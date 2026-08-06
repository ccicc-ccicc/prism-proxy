package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-proxy/internal/config"
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

func TestAuth_ClaudePath(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{AuthKeys: []string{"sk-proxy-1"}},
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL, APIKey: "sk", Format: "claude", Model: "claude-3-5-sonnet"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-proxy-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("bearer on claude path: %d", resp.StatusCode)
	}

	req2, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"messages":[]}`))
	req2.Header.Set("x-api-key", "sk-proxy-1")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("x-api-key on claude path: %d", resp2.StatusCode)
	}
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
