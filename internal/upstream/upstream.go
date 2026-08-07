package upstream

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"prism-proxy/internal/config"
)

type Client struct {
	hc *http.Client
	// transports 按 timeout 缓存 Transport：同一 timeout 的所有请求
	// 共享连接池（连接复用），不同 timeout 互不干扰。
	mu         sync.Mutex
	transports map[time.Duration]*http.Transport
}

func NewClient() *Client {
	transport := &http.Transport{
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
	}
	hc := &http.Client{Transport: transport}
	return &Client{hc: hc, transports: make(map[time.Duration]*http.Transport)}
}

// transportFor 返回指定超时对应的 Transport。首次使用某 timeout 时
// clone 一次并缓存（此后不再修改），后续请求复用同一实例，
// 从而复用其连接池。每个请求 clone 一份会导致连接池无法复用
// （http.Transport.Clone 不复制 idle-conn pool）。
func (c *Client) transportFor(timeout time.Duration) *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transports[timeout]; ok {
		return t
	}
	t := c.hc.Transport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = timeout
	c.transports[timeout] = t
	return t
}

// Do 转发请求。超时语义：连接 + 响应头阶段用 u.Timeout（默认 120s）；
// 流式 body 读取的总时长不限，空闲超时由调用方按块控制。
// 每个 timeout 值对应一个缓存 Transport，其 ResponseHeaderTimeout
// 在首次使用时设置、此后不变，多请求共享连接池（spec §6 连接复用）。
func (c *Client) Do(ctx context.Context, u *config.UpstreamConfig, body []byte, stream bool) (*http.Response, error) {
	endpoint := "/chat/completions"
	if u.Format == "claude" {
		endpoint = "/messages"
	}
	url := strings.TrimSuffix(u.BaseURL, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.Format == "claude" {
		req.Header.Set("x-api-key", u.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}
	timeout := u.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	// ResponseHeaderTimeout 仅覆盖到响应头；body 读取不受限
	client := &http.Client{Transport: c.transportFor(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	return resp, nil
}
