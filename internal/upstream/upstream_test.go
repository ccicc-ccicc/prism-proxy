package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"prism-proxy/internal/config"
)

func TestDo_OpenAIFormatHeaders(t *testing.T) {
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	c := NewClient()
	u := &config.UpstreamConfig{BaseURL: srv.URL + "/v1", APIKey: "sk-1", Format: "openai", Model: "m"}
	resp, err := c.Do(context.Background(), u, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer sk-1" || !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("headers: auth=%q ct=%q", gotAuth, gotCT)
	}
}

func TestDo_ClaudeFormatHeaders(t *testing.T) {
	var gotKey, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_x"}`))
	}))
	defer srv.Close()
	c := NewClient()
	u := &config.UpstreamConfig{BaseURL: srv.URL + "/v1", APIKey: "sk-2", Format: "claude", Model: "m"}
	resp, err := c.Do(context.Background(), u, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotKey != "sk-2" || gotVer != "2023-06-01" {
		t.Fatalf("headers: key=%q ver=%q", gotKey, gotVer)
	}
}

func TestDo_ClaudeFormatAuthOverrideBearer(t *testing.T) {
	var gotAuth, gotKey, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_x"}`))
	}))
	defer srv.Close()
	c := NewClient()
	u := &config.UpstreamConfig{BaseURL: srv.URL + "/v1", APIKey: "sk-2", Format: "claude", Model: "m", Auth: "bearer"}
	resp, err := c.Do(context.Background(), u, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer sk-2" || gotKey != "" || gotVer != "2023-06-01" {
		t.Fatalf("headers: auth=%q key=%q ver=%q", gotAuth, gotKey, gotVer)
	}
}

func TestDo_ErrorPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()
	c := NewClient()
	u := &config.UpstreamConfig{BaseURL: srv.URL + "/v1", APIKey: "k", Format: "openai", Model: "m"}
	resp, err := c.Do(context.Background(), u, []byte(`{}`), false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

// TestDo_ReusesConnections 验证同一 timeout 的连续请求复用 TCP 连接
// （spec §6 连接复用）：两个顺序请求必须只建立 1 条连接。
func TestDo_ReusesConnections(t *testing.T) {
	var newConns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x"}`))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := NewClient()
	u := &config.UpstreamConfig{BaseURL: srv.URL, APIKey: "k", Format: "openai", Model: "m"}
	for i := 0; i < 2; i++ {
		resp, err := c.Do(context.Background(), u, []byte(`{}`), false)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("TCP connections created: %d, want 1 (idle conn must be reused)", got)
	}
}
