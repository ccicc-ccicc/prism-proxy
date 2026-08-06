package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"prism-proxy/internal/config"
	"prism-proxy/internal/route"
	"prism-proxy/internal/upstream"
)

const maxBody = 50 << 20 // 50MB
const streamIdleTimeout = 60 * time.Second

// cfgProvider 是最小的配置读取接口：生产用 *config.Watcher（热加载），
// 测试用 staticWatcher 包一层静态配置，两者都满足。
type cfgProvider interface {
	Get() *config.Config
}

type Server struct {
	cfg    cfgProvider
	client *upstream.Client
	logger *slog.Logger
}

func New(cfg *config.Watcher, client *upstream.Client, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, client: client, logger: logger}
}

// NewWithConfig 测试注入：包一层只有 Get 的 Watcher
func NewWithConfig(cfg *config.Config, client *upstream.Client, logger *slog.Logger) *Server {
	w := &staticWatcher{cfg: cfg}
	return &Server{cfg: w, client: client, logger: logger}
}

type staticWatcher struct{ cfg *config.Config }

func (w *staticWatcher) Get() *config.Config { return w.cfg }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	format := ""
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		format = "openai"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		format = "claude"
	default:
		http.NotFound(w, r)
		return
	}
	cfg := s.cfg.Get()
	if !s.authenticated(r, format, cfg) {
		http.Error(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	decision, err := route.Decide(cfg, format, body)
	if err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.relay(w, r, cfg, format, body, decision); err != nil {
		// 请求解析/转换失败：未写入任何响应，回 400
		s.log(start, format, decision, cfg.Upstreams[decision.Upstream], http.StatusBadRequest, false, err)
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
}

func (s *Server) copyStream(w http.ResponseWriter, r *http.Request, src io.Reader) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, src)
		return err
	}
	buf := make([]byte, 32*1024)
	for {
		ctx, cancel := context.WithTimeout(r.Context(), streamIdleTimeout)
		chunk, err := readWithCtx(ctx, src, buf)
		cancel()
		if len(chunk) > 0 {
			if _, werr := w.Write(chunk); werr != nil {
				return werr // 下游已断开，停止回写
			}
			flusher.Flush()
		}
		if err != nil {
			return err // 断流（EOF 或超时），不伪造结束事件
		}
	}
}

func readWithCtx(ctx context.Context, src io.Reader, buf []byte) ([]byte, error) {
	done := make(chan struct{})
	var n int
	var err error
	go func() {
		n, err = src.Read(buf)
		close(done)
	}()
	select {
	case <-done:
		return buf[:n], err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) authenticated(r *http.Request, format string, cfg *config.Config) bool {
	keys := cfg.Server.AuthKeys
	if len(keys) == 0 {
		return true
	}
	var got string
	if format == "claude" {
		got = r.Header.Get("x-api-key")
	} else {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	for _, k := range keys {
		if got == k {
			return true
		}
	}
	return false
}

func rewriteModel(format string, body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(model)
	m["model"] = b
	return json.Marshal(m)
}

func (s *Server) log(start time.Time, format string, d route.Decision, up config.UpstreamConfig, status int, stream bool, err error) {
	attrs := []any{
		"inbound", format,
		"upstream", d.Upstream,
		"outbound", up.Format,
		"model", d.Model,
		"vision_switch", d.VisionSwitch,
		"stream", stream,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.logger.Info("proxy_request", attrs...)
}
