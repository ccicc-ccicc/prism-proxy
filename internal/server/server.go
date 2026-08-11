package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
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
	if !s.authenticated(r, cfg) {
		writeError(w, format, http.StatusUnauthorized, "invalid api key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, format, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > maxBody {
		writeError(w, format, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	decision, err := route.Decide(cfg, format, body)
	if err != nil {
		writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if decision.Upstream == "main" && cfg.AutoSwitchVision {
		var sanitized bool
		body, sanitized, err = route.SanitizeHistoryImages(format, body)
		if err != nil {
			writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		decision.ImagesSanitized = sanitized
	}
	if err := s.relay(w, r, cfg, format, body, decision); err != nil {
		// 请求转换失败回 400；上游来源失败回 502（读取/解析上游响应）；
		// 上游响应超限回 413。错误信封一律用入站格式（writeError）。
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, errResponseTooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.Is(err, errUpstreamResponse):
			status = http.StatusBadGateway
		}
		msg := "invalid request: " + err.Error()
		if status != http.StatusBadRequest {
			msg = err.Error()
		}
		s.log(start, format, decision, cfg.Upstreams[decision.Upstream], status, false, err)
		writeError(w, format, status, msg)
		return
	}
}

// writeError 以入站协议的错误信封回写错误（spec §5）：
// OpenAI → {"error":{"message":...,"type":...}}；
// Claude → {"type":"error","error":{"type":...,"message":...}}。
// 认证失败用 authentication_error，其余用 invalid_request_error。
func writeError(w http.ResponseWriter, format string, status int, message string) {
	etype := "invalid_request_error"
	if status == http.StatusUnauthorized {
		etype = "authentication_error"
	}
	var payload []byte
	if format == "claude" {
		payload, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": convert.ClaudeError{Type: etype, Message: message},
		})
	} else {
		payload, _ = json.Marshal(convert.ErrorResponse{Error: convert.ErrorDetail{Message: message, Type: etype}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
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

func (s *Server) authenticated(r *http.Request, cfg *config.Config) bool {
	keys := cfg.Server.AuthKeys
	if len(keys) == 0 {
		return true
	}
	// spec §3：两个头都查（Authorization: Bearer 与 x-api-key），任一命中即过，
	// 与入口路径无关。
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	apiKey := r.Header.Get("x-api-key")
	for _, k := range keys {
		if auth == k || apiKey == k {
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
	if d.HasImage && !d.VisionSwitch {
		// spec §7：检测到图片但未切换（开关关/无 vision 上游）时记录
		attrs = append(attrs, "image_detected", true)
	}
	if d.ImagesSanitized {
		attrs = append(attrs, "images_sanitized", true)
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.logger.Info("proxy_request", attrs...)
}
