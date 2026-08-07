// Package trafficlog 实现内容日志：JSONL 每请求一条，记录客户端请求、
// 出站请求、上游响应、出站响应四段内容，脱敏并支持大小轮转。
package trafficlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Redact 将 JSON body 中 api_key / key 字段的字符串值替换为 sk-***。
// 非 JSON body 原样返回；JSON 字段名精确匹配（含嵌套对象与数组）。
func Redact(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "api_key" || k == "key" {
				if _, ok := val.(string); ok {
					t[k] = "sk-***"
				}
			} else {
				redactValue(val)
			}
		}
	case []any:
		for _, e := range t {
			redactValue(e)
		}
	}
}

// maxMem 流式/超长内容内存驻留上限：超过则转日志目录临时文件。
const maxMem = 32 << 20 // 32MB

// segBuffer 分段累积缓冲：≤maxMem 驻留内存，超出转目录下临时文件。
// 单 goroutine 使用（每请求一个），无需锁。
type segBuffer struct {
	dir    string
	name   string // 临时文件前缀
	buf    bytes.Buffer
	file   *os.File
	size   int64
	maxMem int
}

func newSegBuffer(dir, name string) *segBuffer {
	return newSegBufferMax(dir, name, maxMem)
}

func newSegBufferMax(dir, name string, maxMem int) *segBuffer {
	return &segBuffer{dir: dir, name: name, maxMem: maxMem}
}

func (s *segBuffer) Write(p []byte) (int, error) {
	if s.file == nil {
		if s.size+int64(len(p)) <= int64(s.maxMem) {
			n, _ := s.buf.Write(p)
			s.size += int64(n)
			return n, nil
		}
		// 先写入内存缓冲再尝试落盘：落盘失败时数据保留在 buf，不丢失
		n, _ := s.buf.Write(p)
		s.size += int64(n)
		if err := s.spill(); err != nil {
			return n, err
		}
		return n, nil
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	return n, err
}

func (s *segBuffer) spill() error {
	f, err := os.CreateTemp(s.dir, s.name+"-*.part")
	if err != nil {
		return fmt.Errorf("traffic spill: %w", err)
	}
	if s.buf.Len() > 0 {
		if _, err := f.Write(s.buf.Bytes()); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return err
		}
		s.buf.Reset()
	}
	s.file = f
	return nil
}

// String 返回完整内容（内存或临时文件）。
func (s *segBuffer) String() string {
	if s.file == nil {
		return s.buf.String()
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(s.file)
	if err != nil {
		return ""
	}
	return string(data)
}

// Close 关闭并删除临时文件（幂等）。
func (s *segBuffer) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	err := s.file.Close()
	if rmErr := os.Remove(name); err == nil {
		err = rmErr
	}
	s.file = nil
	return err
}

// Entry 单条内容日志（JSONL，每请求一条）。
type Entry struct {
	TS               string `json:"ts"`
	RequestID        string `json:"request_id"`
	Inbound          string `json:"inbound"`
	Upstream         string `json:"upstream"`
	Outbound         string `json:"outbound"`
	Model            string `json:"model"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	DurationMS       int64  `json:"duration_ms"`
	InboundBody      string `json:"inbound_body"`
	OutboundBody     string `json:"outbound_body"`
	UpstreamResponse string `json:"upstream_response"`
	OutboundResponse string `json:"outbound_response"`
	Error            string `json:"error"`
}

// TrafficLog 内容日志 writer：JSONL 追加写，lumberjack 按大小轮转。
// 并发安全（内部 mutex）。
type TrafficLog struct {
	w   io.Writer
	dir string
	mu  sync.Mutex
}

// New 创建 TrafficLog：创建 dir 目录，写 <dir>/traffic.log，100MB 轮转。
func New(dir string, maxFiles int) (*TrafficLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("traffic log dir: %w", err)
	}
	lj := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "traffic.log"),
		MaxSize:    100,      // MB，lumberjack 默认值
		MaxBackups: maxFiles, // 保留旧文件数
	}
	return &TrafficLog{w: lj, dir: dir}, nil
}

// NewWithWriter 测试注入：自定义 writer（如 bytes.Buffer）。
func NewWithWriter(w io.Writer) *TrafficLog {
	return &TrafficLog{w: w}
}

// Dir 返回日志目录（Recorder 临时文件目录复用）。
func (t *TrafficLog) Dir() string { return t.dir }

// WriteEntry 写一条 JSONL。
func (t *TrafficLog) WriteEntry(e Entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err = t.w.Write(data)
	return err
}

// Close 关闭底层 writer（lumberjack）。
func (t *TrafficLog) Close() error {
	if c, ok := t.w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Recorder 每请求的内容日志收集器。四段内容分别累积到 segBuffer，
// 流式走 Writer 旁路（TeeReader/MultiWriter），Entry 时统一脱敏。
type Recorder struct {
	requestID string
	inbound   string
	upstream  string
	outbound  string
	model     string
	start     time.Time
	status    int
	errMsg    string

	inboundBody      *segBuffer
	outboundBody     *segBuffer
	upstreamResponse *segBuffer
	outboundResponse *segBuffer
}

func NewRecorder(dir, requestID, inbound string) *Recorder {
	return &Recorder{
		requestID:        requestID,
		inbound:          inbound,
		start:            time.Now(),
		inboundBody:      newSegBuffer(dir, "inbound-"+requestID),
		outboundBody:     newSegBuffer(dir, "outbound-"+requestID),
		upstreamResponse: newSegBuffer(dir, "upstream-"+requestID),
		outboundResponse: newSegBuffer(dir, "outresp-"+requestID),
	}
}

// RequestID 返回请求 ID（供 slog 关联）。
func (r *Recorder) RequestID() string { return r.requestID }

func (r *Recorder) SetDecision(upstream, outbound, model string) {
	r.upstream, r.outbound, r.model = upstream, outbound, model
}

func (r *Recorder) SetInbound(body []byte)          { _, _ = r.inboundBody.Write(body) }
func (r *Recorder) SetOutbound(body []byte)         { _, _ = r.outboundBody.Write(body) }
func (r *Recorder) SetUpstreamResponse(body []byte) { _, _ = r.upstreamResponse.Write(body) }
func (r *Recorder) SetOutboundResponse(body []byte) { _, _ = r.outboundResponse.Write(body) }
func (r *Recorder) SetError(err error)              { if err != nil { r.errMsg = err.Error() } }
func (r *Recorder) SetStatus(status int)            { r.status = status }

// UpstreamWriter / OutboundWriter 供流式旁路累积（TeeReader/MultiWriter）。
func (r *Recorder) UpstreamWriter() io.Writer { return r.upstreamResponse }
func (r *Recorder) OutboundWriter() io.Writer { return r.outboundResponse }

// Entry 组装单条日志：四段统一脱敏；outbound_response 为空时回退取
// upstream_response（同格式透传路径内容相同）。
func (r *Recorder) Entry(stream bool, duration time.Duration) Entry {
	outboundResp := string(Redact([]byte(r.outboundResponse.String())))
	if outboundResp == "" {
		outboundResp = string(Redact([]byte(r.upstreamResponse.String())))
	}
	return Entry{
		TS:               r.start.Format(time.RFC3339),
		RequestID:        r.requestID,
		Inbound:          r.inbound,
		Upstream:         r.upstream,
		Outbound:         r.outbound,
		Model:            r.model,
		Stream:           stream,
		Status:           r.status,
		DurationMS:       duration.Milliseconds(),
		InboundBody:      string(Redact([]byte(r.inboundBody.String()))),
		OutboundBody:     string(Redact([]byte(r.outboundBody.String()))),
		UpstreamResponse: string(Redact([]byte(r.upstreamResponse.String()))),
		OutboundResponse: outboundResp,
		Error:            r.errMsg,
	}
}

// Close 清理四段临时文件。
func (r *Recorder) Close() {
	r.inboundBody.Close()
	r.outboundBody.Close()
	r.upstreamResponse.Close()
	r.outboundResponse.Close()
}
