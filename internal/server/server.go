package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
	"prism-proxy/internal/route"
	"prism-proxy/internal/trafficlog"
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
	cfg     cfgProvider
	client  *upstream.Client
	logger  *slog.Logger
	traffic *trafficlog.TrafficLog // nil = 不记录内容日志
}

func New(cfg *config.Watcher, client *upstream.Client, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, client: client, logger: logger}
}

// NewWithConfig 测试注入：包一层只有 Get 的 Watcher
func NewWithConfig(cfg *config.Config, client *upstream.Client, logger *slog.Logger) *Server {
	w := &staticWatcher{cfg: cfg}
	return &Server{cfg: w, client: client, logger: logger}
}

// SetTrafficLog 注入内容日志（main 启动时调用；测试可注入内存 writer）。
func (s *Server) SetTrafficLog(tl *trafficlog.TrafficLog) { s.traffic = tl }

// newRequestID 生成短请求 ID：低 32 位纳秒时间戳 + 4 字节随机 hex。
func newRequestID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x", time.Now().UnixNano()&0xffffffff, b)
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
		writeError(w, format, http.StatusUnauthorized, "invalid api key", nil)
		return
	}
	// 内容日志（仅 enabled）：每请求 recorder，defer 统一 flush
	var rec *trafficlog.Recorder
	stream := false
	if s.traffic != nil && cfg.Logging.Enabled {
		rid := newRequestID()
		w.Header().Set("X-Request-Id", rid)
		rec = trafficlog.NewRecorder(s.traffic.Dir(), rid, format)
		defer func() { s.flushTraffic(rec, start, stream) }()
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		if rec != nil {
			rec.SetError(fmt.Errorf("read body: %w", err))
			rec.SetStatus(http.StatusBadRequest)
		}
		writeError(w, format, http.StatusBadRequest, "read body: "+err.Error(), rec)
		return
	}
	if len(body) > maxBody {
		if rec != nil {
			rec.SetError(fmt.Errorf("request too large: %d bytes exceeds %d", len(body), maxBody))
			rec.SetStatus(http.StatusRequestEntityTooLarge)
		}
		writeError(w, format, http.StatusRequestEntityTooLarge, "request too large", rec)
		return
	}
	stream = requestStream(format, body)
	if rec != nil {
		rec.SetInbound(body)
	}
	decision, err := route.Decide(cfg, format, body)
	if err != nil {
		if rec != nil {
			rec.SetError(err)
			rec.SetStatus(http.StatusBadRequest)
		}
		writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error(), rec)
		return
	}
	if decision.NeedPreprocess {
		// vision 预处理：最新 run 图片经 vision 上游单独解析为文本后替换回请求，
		// 再走 main（vision 只收图、main 只收文本）。
		vpStart := time.Now()
		body, err = s.preprocessVision(r.Context(), cfg, format, body)
		decision.VisionPreprocess = true
		decision.VisionPreprocessMs = time.Since(vpStart).Milliseconds()
		if err != nil {
			// 请求侧（提取/构造/解析）失败 → 400；vision 上游失败 → 502
			status := http.StatusBadRequest
			if errors.Is(err, errVisionUpstream) {
				status = http.StatusBadGateway
			}
			if rec != nil {
				rec.SetError(err)
				rec.SetStatus(status)
			}
			writeError(w, format, status, err.Error(), rec)
			return
		}
	}
	if decision.Upstream == "main" && cfg.AutoSwitchVision {
		var sanitized bool
		body, sanitized, err = route.SanitizeHistoryImages(format, body)
		if err != nil {
			if rec != nil {
				rec.SetError(err)
				rec.SetStatus(http.StatusBadRequest)
			}
			writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error(), rec)
			return
		}
		decision.ImagesSanitized = sanitized
	}
	if rec != nil {
		up := cfg.Upstreams[decision.Upstream]
		rec.SetDecision(decision.Upstream, up.Format, decision.Model)
	}
	if err := s.relay(w, r, cfg, format, body, decision, rec); err != nil {
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
		if rec != nil {
			rec.SetError(err)
			rec.SetStatus(status)
		}
		s.log(start, format, decision, cfg.Upstreams[decision.Upstream], status, stream, err, rec)
		writeError(w, format, status, msg, rec)
		return
	}
	if rec != nil {
		rec.SetStatus(http.StatusOK)
	}
}

// flushTraffic 将 recorder 组装为条目写入内容日志；失败不阻塞请求。
func (s *Server) flushTraffic(rec *trafficlog.Recorder, start time.Time, stream bool) {
	defer rec.Close()
	entry := rec.Entry(stream, time.Since(start))
	if err := s.traffic.WriteEntry(entry); err != nil {
		s.logger.Error("traffic log write failed", "request_id", entry.RequestID, "err", err)
	}
}

// writeError 以入站协议的错误信封回写错误（spec §5）：
// OpenAI → {"error":{"message":...,"type":...}}；
// Claude → {"type":"error","error":{"type":...,"message":...}}。
// 认证失败用 authentication_error，其余用 invalid_request_error。
// rec 非 nil 时同时记录信封为出站响应。
func writeError(w http.ResponseWriter, format string, status int, message string, rec *trafficlog.Recorder) {
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
	if rec != nil {
		rec.SetStatus(status)
		rec.SetOutboundResponse(payload)
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

func (s *Server) log(start time.Time, format string, d route.Decision, up config.UpstreamConfig, status int, stream bool, err error, rec *trafficlog.Recorder) {
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
	if rec != nil {
		attrs = append(attrs, "request_id", rec.RequestID())
	}
	if d.HasImage && !d.VisionSwitch && !d.NeedPreprocess {
		// spec §7：检测到图片但未切换（开关关/无 vision 上游）时记录；
		// 预处理请求已输出 vision_preprocess 标记，避免冗余
		attrs = append(attrs, "image_detected", true)
	}
	if d.VisionPreprocess {
		attrs = append(attrs, "vision_preprocess", true, "vision_preprocess_ms", d.VisionPreprocessMs)
	}
	if d.ImagesSanitized {
		attrs = append(attrs, "images_sanitized", true)
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.logger.Info("proxy_request", attrs...)
}
