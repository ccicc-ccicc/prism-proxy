package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
	"prism-proxy/internal/route"
)

// relay 处理单个请求的完整转发。format 为入站格式，up.Format 为出站格式：
// 同格式透传，交叉格式先转换请求再转发，响应按流式/非流式反向转换。
func (s *Server) relay(w http.ResponseWriter, r *http.Request, cfg *config.Config, format string, body []byte, d route.Decision) error {
	up := cfg.Upstreams[d.Upstream]
	stream := requestStream(format, body)

	// 同格式：透传
	if format == up.Format {
		outbound, err := rewriteModel(format, body, d.Model)
		if err != nil {
			return err
		}
		return s.forward(w, r, &up, outbound, format, d)
	}
	// 交叉格式：转换请求
	var outbound []byte
	if format == "openai" {
		var req convert.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("parse: %w", err)
		}
		claudeReq, err := convert.OpenAIRequestToClaude(&req, d.Model)
		if err != nil {
			return err
		}
		claudeReq.Stream = stream
		if err := fetchExternalImages(claudeReq); err != nil {
			return err
		}
		outbound, err = json.Marshal(claudeReq)
		if err != nil {
			return err
		}
	} else {
		var req convert.MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("parse: %w", err)
		}
		openaiReq, err := convert.ClaudeRequestToOpenAI(&req, d.Model)
		if err != nil {
			return err
		}
		openaiReq.Stream = stream
		outbound, err = json.Marshal(openaiReq)
		if err != nil {
			return err
		}
	}
	return s.forward(w, r, &up, outbound, format, d)
}

// forward 转发并转换响应。上游非 2xx 原样回写（不转换）；同格式透传
// （流式走 copyStream）；交叉格式按流式/非流式转换。返回的 error 表示
// 请求/转换失败（由调用方回 400），上游类错误已在内部回写并记日志。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, up *config.UpstreamConfig, outbound []byte, inboundFormat string, d route.Decision) error {
	start := time.Now()
	resp, err := s.client.Do(r.Context(), up, outbound, false)
	if err != nil {
		s.log(start, inboundFormat, d, *up, http.StatusBadGateway, false, err)
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return nil
	}
	defer resp.Body.Close()
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	// 上游非 2xx：错误体原样透传（绝不转换）
	if resp.StatusCode >= 400 {
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		s.log(start, inboundFormat, d, *up, resp.StatusCode, isStream, nil)
		return nil
	}
	// 同格式：透传（头部原样复制，流式/非流式都保持 Task 12 语义）
	if inboundFormat == up.Format {
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		var copyErr error
		if isStream {
			copyErr = s.copyStream(w, r, resp.Body)
		} else {
			_, copyErr = io.Copy(w, resp.Body)
		}
		if errors.Is(copyErr, io.EOF) {
			copyErr = nil // 流式自然结束，非错误
		}
		s.log(start, inboundFormat, d, *up, resp.StatusCode, isStream, copyErr)
		return nil
	}
	// 交叉格式：转换响应
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.StatusCode)
		cerr := s.convertStream(r.Context(), w, resp.Body, inboundFormat, up.Format, up.Model)
		// 成功与中途断流/转换失败都记日志：成功一行（status 200），
		// 失败停止转发（已完成回写的部分保留），不伪造结束事件
		s.log(start, inboundFormat, d, *up, resp.StatusCode, true, cerr)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("read upstream response: %w", err)
	}
	if len(data) > maxBody {
		return fmt.Errorf("upstream response too large")
	}
	var out any
	if inboundFormat == "openai" { // 上游 Claude → 客户端 OpenAI
		var cr convert.MessagesResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return fmt.Errorf("parse claude response: %w", err)
		}
		o, err := convert.ClaudeResponseToOpenAI(&cr, "")
		if err != nil {
			return err
		}
		o.Created = time.Now().Unix()
		out = o
	} else { // 上游 OpenAI → 客户端 Claude
		var or convert.ChatCompletionResponse
		if err := json.Unmarshal(data, &or); err != nil {
			return fmt.Errorf("parse openai response: %w", err)
		}
		o, err := convert.OpenAIResponseToClaude(&or, "")
		if err != nil {
			return err
		}
		out = o
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.log(start, inboundFormat, d, *up, resp.StatusCode, false, err)
		return nil
	}
	s.log(start, inboundFormat, d, *up, resp.StatusCode, false, nil)
	return nil
}

// convertStream 将上游 SSE 流逐帧转换为对端格式并回写：
//   - outboundFormat == "claude"（上游 Claude，客户端 OpenAI）：C2OStream 逐帧转换，
//     流结束（done）时回写 Close() 的 [DONE]；中途错误返回 error（不写 [DONE]）；
//   - outboundFormat == "openai"（上游 OpenAI，客户端 Claude）：O2CStream 按 data 行转换，
//     正常结束（[DONE] 或 EOF）回写 Finish() 的结束帧；中途错误返回 error，不发任何结束帧。
//
// 流式读受 streamIdleTimeout 空闲超时约束：上游挂起时读返回超时错误，停止转发。
func (s *Server) convertStream(ctx context.Context, w http.ResponseWriter, src io.Reader, inboundFormat, outboundFormat, model string) error {
	src = newTimeoutReader(ctx, src, streamIdleTimeout)
	if outboundFormat == "claude" {
		// OpenAI 客户端收 Claude 流 → C2O
		c := convert.NewC2OStream(model)
		return s.readClaudeFrames(src, func(frame []byte) error {
			outs, err := c.Write(frame)
			if err != nil {
				return err
			}
			fl, _ := w.(http.Flusher)
			for _, o := range outs {
				_, _ = w.Write(o)
			}
			if fl != nil {
				fl.Flush()
			}
			return nil
		}, func() error {
			for _, o := range c.Close() {
				_, _ = w.Write(o)
			}
			fl, _ := w.(http.Flusher)
			if fl != nil {
				fl.Flush()
			}
			return nil
		})
	}
	// 客户端 Claude 收 OpenAI 流 → O2C
	c := convert.NewO2CStreamWithModel(model)
	scanner := &openAISSE{}
	var streamErr error
	for {
		data, done, err := scanner.Next(src)
		if done {
			break
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err // 断流/读超时：不伪造结束帧
			}
			break
		}
		outs, err := c.Write(data)
		if err != nil {
			streamErr = err // chunk 解析失败：不伪造结束帧
			break
		}
		fl, _ := w.(http.Flusher)
		for _, o := range outs {
			_, _ = w.Write(o)
		}
		if fl != nil {
			fl.Flush()
		}
	}
	// 仅正常结束才发 message_stop；中途错误静默断流（由调用方记日志）
	if streamErr == nil {
		for _, o := range c.Finish() {
			_, _ = w.Write(o)
		}
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
	}
	return streamErr
}

// readClaudeFrames 逐帧读取 Claude SSE 流并回调；流正常结束调用 onDone，
// 断流/解析失败返回 error（不回调 onDone，不伪造结束事件）。
func (s *Server) readClaudeFrames(src io.Reader, onFrame func([]byte) error, onDone func() error) error {
	reader := newSSEReader(src)
	for {
		frame, done, err := reader.Next()
		if done {
			return onDone()
		}
		if err != nil {
			return err // 断流
		}
		if len(frame) == 0 {
			continue
		}
		if err := onFrame(frame); err != nil {
			return err
		}
	}
}

// timeoutReader 给流式读加空闲超时：每次 Read 不超过 timeout 即返回
// 超时错误（复用 readWithCtx 的逐块超时模式），防止上游挂起时
// handler 被 bufio.Scanner 永久阻塞。与 copyStream 相同的超时语义。
type timeoutReader struct {
	ctx     context.Context
	timeout time.Duration
	src     io.Reader
}

func newTimeoutReader(ctx context.Context, src io.Reader, timeout time.Duration) io.Reader {
	return &timeoutReader{ctx: ctx, timeout: timeout, src: src}
}

func (r *timeoutReader) Read(p []byte) (int, error) {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	b, err := readWithCtx(ctx, r.src, p)
	return len(b), err
}

// requestStream 从请求 body 提取 stream 字段。
func requestStream(format string, body []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	var stream bool
	_ = json.Unmarshal(m["stream"], &stream)
	return stream
}

// fetchExternalImages 将请求中 Source.Type=="url" 的图片 block 下载为 base64。
// 兼容两种 content 形状：[]convert.ClaudeBlock 与 []any（map，如经
// json.Marshal/Unmarshal 往返后的请求）。下载失败返回 error（由调用方回 400）。
func fetchExternalImages(req *convert.MessagesRequest) error {
	for mi, msg := range req.Messages {
		switch blocks := msg.Content.(type) {
		case []convert.ClaudeBlock:
			for bi, b := range blocks {
				if b.Type != "image" || b.Source == nil || b.Source.Type != "url" {
					continue
				}
				src, err := downloadImage(b.Source.Data)
				if err != nil {
					return err
				}
				blocks[bi].Source = src
			}
			req.Messages[mi].Content = blocks
		case []any:
			for bi, p := range blocks {
				m, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if m["type"] != "image" {
					continue
				}
				srcMap, ok := m["source"].(map[string]any)
				if !ok || srcMap["type"] != "url" {
					continue
				}
				urlStr, _ := srcMap["data"].(string)
				src, err := downloadImage(urlStr)
				if err != nil {
					return err
				}
				m["source"] = map[string]any{
					"type":       "base64",
					"media_type": src.MediaType,
					"data":       src.Data,
				}
				blocks[bi] = m
			}
			req.Messages[mi].Content = blocks
		}
	}
	return nil
}

// downloadImage 下载外链图片（仅 http/https），返回 base64 ImageSource。
func downloadImage(url string) (*convert.ImageSource, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("unsupported image url scheme")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download image: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("image too large")
	}
	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		return nil, fmt.Errorf("image content-type missing")
	}
	return &convert.ImageSource{Type: "base64", MediaType: mediaType, Data: base64Std(data)}, nil
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// sseReader 逐行累积 SSE 帧：空行（\n\n）为帧边界，产出不含末尾空行的完整帧。
type sseReader struct {
	scanner *bufio.Scanner
}

func newSSEReader(src io.Reader) *sseReader {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), maxBody)
	return &sseReader{scanner: sc}
}

// Next 返回下一帧；done=true 表示流正常结束（EOF 且无未决行）。
func (r *sseReader) Next() ([]byte, bool, error) {
	var frame []byte
	for r.scanner.Scan() {
		line := strings.TrimRight(r.scanner.Text(), "\r")
		if line == "" { // 帧边界
			if len(frame) > 0 {
				return frame, false, nil
			}
			continue
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
	}
	if err := r.scanner.Err(); err != nil {
		return nil, false, err
	}
	if len(frame) > 0 {
		return frame, false, nil // EOF 前的最后一帧
	}
	return nil, true, nil
}

// openAISSE 逐条读取 OpenAI SSE 的 data: 行；读到 [DONE] 返回 done=true。
// scanner 跨调用复用，避免缓冲区中已读未消费的行丢失。
type openAISSE struct {
	scanner *bufio.Scanner
}

func (o *openAISSE) Next(src io.Reader) ([]byte, bool, error) {
	if o.scanner == nil {
		o.scanner = bufio.NewScanner(src)
		o.scanner.Buffer(make([]byte, 64*1024), maxBody)
	}
	for o.scanner.Scan() {
		line := strings.TrimSpace(o.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 空行 / 注释行
		}
		if !strings.HasPrefix(line, "data:") {
			continue // 忽略 event: 等其他行
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil, true, nil
		}
		return []byte(data), false, nil
	}
	if err := o.scanner.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, io.EOF
}
