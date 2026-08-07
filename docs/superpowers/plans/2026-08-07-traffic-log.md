# 默认配置路径 + 内容日志实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** serve 缺省使用 `~/.prism-proxy/settings.yaml`（不存在 fail-fast 并提示旧默认）；新增可开关的内容日志（traffic log），lumberjack 大小轮转，JSONL 每请求一条完整四段内容并脱敏。

**Architecture:** 新增 `internal/trafficlog` 包（TrafficLog writer 封装、Entry 序列化、Redact 脱敏、segBuffer 分段累积缓冲）；Server 通过 `SetTrafficLog` 注入，每请求创建 Recorder 收集四段内容，流式用 TeeReader/MultiWriter 旁路，defer 统一 flush；config 新增 `logging` 段；main 默认路径展开 + logging 初始化。

**Tech Stack:** Go 1.25.5、cobra、slog、lumberjack v2（新增依赖）、fsnotify（现有）

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/config/config.go` | 新增 `LoggingConfig{Enabled, Dir, MaxFiles}`、默认值、`~` 展开 |
| `internal/config/config_test.go` | logging 解析/默认值测试 |
| `internal/trafficlog/trafficlog.go` | `TrafficLog`（writer 注入）、`Entry`、`Recorder`、`Redact`、`segBuffer` |
| `internal/trafficlog/trafficlog_test.go` | 上述组件测试 |
| `internal/server/server.go` | `SetTrafficLog`、request_id、`X-Request-Id` 头、recorder 创建/flush、writeError 记录、`log` 加 request_id |
| `internal/server/convert.go` | relay/forward 记录四段、上游 ≥400 透传旁路、流式旁路（TeeReader/MultiWriter） |
| `internal/server/server_test.go` | 内容日志集成测试（新增，不改现有测试） |
| `cmd/prism-proxy/main.go` | 默认配置路径展开 + 兼容错误提示 + logging 初始化 |
| `cmd/prism-proxy/main_test.go` | `resolveConfigPath` 测试（新增） |
| `README.md`、`prism-proxy.yaml.example` | 文档更新 |
| `go.mod` | 新增 lumberjack 依赖 |

---

## Task 1: lumberjack 依赖 + config.LoggingConfig

**Files:**
- Modify: `go.mod`、`internal/config/config.go`、`internal/config/config_test.go`

- [ ] **Step 1: 添加 lumberjack 依赖**

Run: `go get gopkg.in/natefinch/lumberjack.v2@latest`
Expected: go.mod 增加 `gopkg.in/natefinch/lumberjack.v2` require

- [ ] **Step 2: 写失败测试**

在 `internal/config/config_test.go` 末尾追加：

```go
func TestLogging_DefaultsAndParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("main:\n  baseurl: http://x/v1\n  api_key: sk\n  format: openai\n  model: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Enabled {
		t.Error("logging.enabled want default false")
	}
	home, _ := os.UserHomeDir()
	wantDir := filepath.Join(home, ".prism-proxy", "logs")
	if cfg.Logging.Dir != wantDir {
		t.Errorf("logging.dir=%q want %q", cfg.Logging.Dir, wantDir)
	}
	if cfg.Logging.MaxFiles != 3 {
		t.Errorf("logging.max_files=%d want 3", cfg.Logging.MaxFiles)
	}
}

func TestLogging_ParseExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("main:\n  baseurl: http://x/v1\n  api_key: sk\n  format: openai\n  model: m\nlogging:\n  enabled: true\n  dir: ~/custom/logs\n  max_files: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Logging.Enabled {
		t.Error("logging.enabled want true")
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, "custom", "logs"); cfg.Logging.Dir != want {
		t.Errorf("dir=%q want %q", cfg.Logging.Dir, want)
	}
	if cfg.Logging.MaxFiles != 5 {
		t.Errorf("max_files=%d want 5", cfg.Logging.MaxFiles)
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestLogging -v`
Expected: FAIL（`cfg.Logging` 字段不存在）

- [ ] **Step 4: 实现**

`internal/config/config.go` 修改：

```go
type Config struct {
	Server           ServerConfig              `yaml:"server"`
	AutoSwitchVision bool                      `yaml:"auto_switch_vision"`
	Logging          LoggingConfig             `yaml:"logging"`
	Upstreams        map[string]UpstreamConfig `yaml:",inline"`
}

// LoggingConfig 内容日志配置（traffic log）。
type LoggingConfig struct {
	Enabled bool   `yaml:"enabled"`   // 默认 false
	Dir     string `yaml:"dir"`       // 默认 ~/.prism-proxy/logs，支持 ~ 前缀
	MaxFiles int   `yaml:"max_files"` // 轮转保留旧文件数；0 = 不删除
}
```

`applyDefaults` 中追加（在 Server.Listen 默认之后）：

```go
	if c.Logging.Dir == "" {
		c.Logging.Dir = "~/.prism-proxy/logs"
	}
	if c.Logging.MaxFiles == 0 {
		c.Logging.MaxFiles = 3
	}
	c.Logging.Dir = expandHome(c.Logging.Dir)
```

追加包级函数：

```go
// expandHome 展开 ~ 前缀为用户主目录；非 ~ 开头原样返回。
func expandHome(path string) string {
	if path == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[2:])
		}
	}
	return path
}
```

确保 config.go 的 import 增加 `path/filepath` 和 `strings`。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: PASS（含原有测试）

- [ ] **Step 6: 提交**

```bash
git add go.mod go.sum internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): 新增 logging 配置段（enabled/dir/max_files）与默认值"
```

---

## Task 2: trafficlog.Redact 脱敏

**Files:**
- Create: `internal/trafficlog/trafficlog.go`
- Create: `internal/trafficlog/trafficlog_test.go`

- [ ] **Step 1: 写失败测试**

`internal/trafficlog/trafficlog_test.go`：

```go
package trafficlog

import (
	"strings"
	"testing"
)

func TestRedact_ApiKeyNested(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"api_key":"sk-secret-123"}]}`)
	out := Redact(in)
	if strings.Contains(string(out), "sk-secret-123") {
		t.Fatalf("leaked api_key: %s", out)
	}
	if !strings.Contains(string(out), "sk-***") {
		t.Fatalf("no mask: %s", out)
	}
}

func TestRedact_KeyField(t *testing.T) {
	out := Redact([]byte(`{"key":"abc123","name":"x"}`))
	if strings.Contains(string(out), "abc123") {
		t.Fatalf("leaked key: %s", out)
	}
	if !strings.Contains(string(out), "sk-***") {
		t.Fatalf("no mask: %s", out)
	}
}

func TestRedact_NonJSON(t *testing.T) {
	in := []byte("not json at all")
	if string(Redact(in)) != string(in) {
		t.Fatalf("non-json changed: %s", Redact(in))
	}
}

func TestRedact_ArrayAndNormalFields(t *testing.T) {
	in := []byte(`[{"key":"k1","content":"keep me"},{"nested":{"api_key":"sk-x"}}]`)
	out := Redact(in)
	s := string(out)
	if strings.Contains(s, "k1") || strings.Contains(s, "sk-x") {
		t.Fatalf("leaked: %s", s)
	}
	if !strings.Contains(s, "keep me") {
		t.Fatalf("normal content lost: %s", s)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/trafficlog/ -run TestRedact -v`
Expected: FAIL（包不存在）

- [ ] **Step 3: 实现**

`internal/trafficlog/trafficlog.go`：

```go
// Package trafficlog 实现内容日志：JSONL 每请求一条，记录客户端请求、
// 出站请求、上游响应、出站响应四段内容，脱敏并支持大小轮转。
package trafficlog

import (
	"encoding/json"
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/trafficlog/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/trafficlog/
git commit -m "feat(trafficlog): 实现 Redact 脱敏（api_key/key 字段掩码）"
```

---

## Task 3: trafficlog.segBuffer 分段累积缓冲

**Files:**
- Modify: `internal/trafficlog/trafficlog.go`、`internal/trafficlog/trafficlog_test.go`

- [ ] **Step 1: 写失败测试**

`internal/trafficlog/trafficlog_test.go` 追加：

```go
import (
	"bytes"
	"os"
	"path/filepath"
)

func TestSegBuffer_InMemory(t *testing.T) {
	s := newSegBufferMax(t.TempDir(), "seg", 1024)
	defer s.Close()
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if s.String() != "hello" {
		t.Fatalf("got %q", s.String())
	}
	if s.file != nil {
		t.Fatal("want in-memory, got file")
	}
}

func TestSegBuffer_SpillToDisk(t *testing.T) {
	dir := t.TempDir()
	s := newSegBufferMax(dir, "seg", 8)
	defer s.Close()
	big := bytes.Repeat([]byte("a"), 100)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if s.file == nil {
		t.Fatal("want spilled to file")
	}
	if s.String() != string(big) {
		t.Fatalf("content mismatch: len %d want %d", len(s.String()), len(big))
	}
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".part" {
			found = true
		}
	}
	if !found {
		t.Fatal("no .part temp file")
	}
}

func TestSegBuffer_CloseRemovesFile(t *testing.T) {
	dir := t.TempDir()
	s := newSegBufferMax(dir, "seg", 8)
	_, _ = s.Write(bytes.Repeat([]byte("b"), 100))
	name := s.file.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/trafficlog/ -run TestSegBuffer -v`
Expected: FAIL（`newSegBufferMax` 未定义）

- [ ] **Step 3: 实现**

`internal/trafficlog/trafficlog.go` 追加：

```go
import (
	"bytes"
	"fmt"
	"io"
	"os"
)

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
		if err := s.spill(); err != nil {
			return 0, err
		}
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/trafficlog/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/trafficlog/
git commit -m "feat(trafficlog): 实现 segBuffer 分段累积（32MB 内存封顶，超限落盘）"
```

---

## Task 4: trafficlog.TrafficLog + Entry

**Files:**
- Modify: `internal/trafficlog/trafficlog.go`、`internal/trafficlog/trafficlog_test.go`

- [ ] **Step 1: 写失败测试**

`internal/trafficlog/trafficlog_test.go` 追加：

```go
import (
	"encoding/json"
	"io"
)

func TestTrafficLog_WriteEntry(t *testing.T) {
	var buf bytes.Buffer
	tl := NewWithWriter(&buf)
	e := Entry{
		TS: "2026-08-07T10:00:00+08:00", RequestID: "rid1", Inbound: "openai",
		Upstream: "main", Outbound: "claude", Model: "m", Stream: false,
		Status: 200, DurationMS: 123, InboundBody: `{"a":1}`,
		UpstreamResponse: `{"b":2}`, Error: "",
	}
	if err := tl.WriteEntry(e); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatal(err)
	}
	if got["request_id"] != "rid1" || got["status"] != float64(200) {
		t.Fatalf("entry: %s", lines[0])
	}
	if got["inbound_body"] != `{"a":1}` {
		t.Fatalf("inbound_body: %v", got["inbound_body"])
	}
}

func TestTrafficLog_ConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	tl := NewWithWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = tl.WriteEntry(Entry{RequestID: "r" + string(rune('0'+n)), TS: "t"})
		}(i % 10)
	}
	wg.Wait()
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
}

func TestTrafficLog_NewCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	tl, err := New(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/trafficlog/ -run TestTrafficLog -v`
Expected: FAIL（`TrafficLog` 未定义）

- [ ] **Step 3: 实现**

`internal/trafficlog/trafficlog.go` 追加：

```go
import (
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/trafficlog/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/trafficlog/
git commit -m "feat(trafficlog): 实现 TrafficLog（JSONL 写入 + lumberjack 轮转）与 Entry"
```

---

## Task 5: trafficlog.Recorder

**Files:**
- Modify: `internal/trafficlog/trafficlog.go`、`internal/trafficlog/trafficlog_test.go`

- [ ] **Step 1: 写失败测试**

`internal/trafficlog/trafficlog_test.go` 追加：

```go
func TestRecorder_EntryAssembly(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid-1", "openai")
	defer rec.Close()
	rec.SetDecision("main", "claude", "claude-3")
	rec.SetInbound([]byte(`{"api_key":"sk-secret","messages":[{"role":"user","content":"hi"}]}`))
	rec.SetOutbound([]byte(`{"model":"claude-3"}`))
	rec.SetUpstreamResponse([]byte(`{"id":"m1"}`))
	rec.SetOutboundResponse([]byte(`{"id":"c1"}`))
	rec.SetStatus(200)
	entry := rec.Entry(false, 500*time.Millisecond)
	if entry.RequestID != "rid-1" || entry.Inbound != "openai" || entry.Upstream != "main" || entry.Outbound != "claude" || entry.Model != "claude-3" {
		t.Fatalf("meta: %+v", entry)
	}
	if entry.DurationMS != 500 {
		t.Fatalf("duration_ms=%d want 500", entry.DurationMS)
	}
	if !strings.Contains(entry.InboundBody, "sk-***") || strings.Contains(entry.InboundBody, "sk-secret") {
		t.Fatalf("inbound not redacted: %s", entry.InboundBody)
	}
	if entry.UpstreamResponse != `{"id":"m1"}` {
		t.Fatalf("upstream_response: %s", entry.UpstreamResponse)
	}
}

func TestRecorder_StreamWriters(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid-2", "claude")
	defer rec.Close()
	rec.SetDecision("main", "openai", "gpt-4o")
	rec.SetStatus(200)
	_, _ = rec.UpstreamWriter().Write([]byte("data: {\"a\":1}\n\ndata: [DONE]\n"))
	_, _ = rec.OutboundWriter().Write([]byte("event: message_start\n\nevent: message_stop\n"))
	entry := rec.Entry(true, 10*time.Millisecond)
	if entry.UpstreamResponse != "data: {\"a\":1}\n\ndata: [DONE]\n" {
		t.Fatalf("upstream stream: %q", entry.UpstreamResponse)
	}
	if entry.OutboundResponse != "event: message_start\n\nevent: message_stop\n" {
		t.Fatalf("outbound stream: %q", entry.OutboundResponse)
	}
}

func TestRecorder_OutboundFallbackToUpstream(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid-3", "openai")
	defer rec.Close()
	rec.SetDecision("main", "openai", "gpt-4o")
	rec.SetUpstreamResponse([]byte(`{"choices":[]}`))
	entry := rec.Entry(false, 0)
	if entry.OutboundResponse != `{"choices":[]}` {
		t.Fatalf("outbound fallback: %q", entry.OutboundResponse)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/trafficlog/ -run TestRecorder -v`
Expected: FAIL（`NewRecorder` 未定义）

- [ ] **Step 3: 实现**

`internal/trafficlog/trafficlog.go` 追加：

```go
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

func (r *Recorder) SetInbound(body []byte)            { _, _ = r.inboundBody.Write(body) }
func (r *Recorder) SetOutbound(body []byte)           { _, _ = r.outboundBody.Write(body) }
func (r *Recorder) SetUpstreamResponse(body []byte)   { _, _ = r.upstreamResponse.Write(body) }
func (r *Recorder) SetOutboundResponse(body []byte)   { _, _ = r.outboundResponse.Write(body) }
func (r *Recorder) SetError(err error)                { if err != nil { r.errMsg = err.Error() } }
func (r *Recorder) SetStatus(status int)              { r.status = status }

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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/trafficlog/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/trafficlog/
git commit -m "feat(trafficlog): 实现 Recorder 四段收集与 Entry 组装（统一脱敏）"
```

---

## Task 6: server 集成——request_id、非流式四段记录、writeError

**Files:**
- Modify: `internal/server/server.go`、`internal/server/convert.go`、`internal/server/server_test.go`

- [ ] **Step 1: 写失败测试**

`internal/server/server_test.go` 末尾追加：

```go
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestTrafficLog_NonStreaming -v`
Expected: FAIL（`SetTrafficLog` 未定义 / `trafficlog` 未导入）

- [ ] **Step 3: 实现 server.go**

`internal/server/server.go`：

import 追加 `crypto/rand`、`fmt`、`prism-proxy/internal/trafficlog`。

```go
type Server struct {
	cfg     cfgProvider
	client  *upstream.Client
	logger  *slog.Logger
	traffic *trafficlog.TrafficLog // nil = 不记录内容日志
}

// SetTrafficLog 注入内容日志（main 启动时调用；测试可注入内存 writer）。
func (s *Server) SetTrafficLog(tl *trafficlog.TrafficLog) { s.traffic = tl }

// newRequestID 生成短请求 ID：低 32 位纳秒时间戳 + 4 字节随机 hex。
func newRequestID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x", time.Now().UnixNano()&0xffffffff, b)
}
```

`ServeHTTP` 重构（整函数替换）：

```go
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
			rec.SetError(errResponseTooLarge)
			rec.SetStatus(http.StatusRequestEntityTooLarge)
		}
		writeError(w, format, http.StatusRequestEntityTooLarge, "request too large", rec)
		return
	}
	stream = requestStream(format, body)
	decision, err := route.Decide(cfg, format, body)
	if err != nil {
		if rec != nil {
			rec.SetError(err)
			rec.SetStatus(http.StatusBadRequest)
		}
		writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error(), rec)
		return
	}
	if rec != nil {
		up := cfg.Upstreams[decision.Upstream]
		rec.SetDecision(decision.Upstream, up.Format, decision.Model)
		rec.SetInbound(body)
	}
	if err := s.relay(w, r, cfg, format, body, decision, rec); err != nil {
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
```

追加 `flushTraffic`：

```go
// flushTraffic 将 recorder 组装为条目写入内容日志；失败不阻塞请求。
func (s *Server) flushTraffic(rec *trafficlog.Recorder, start time.Time, stream bool) {
	defer rec.Close()
	entry := rec.Entry(stream, time.Since(start))
	if err := s.traffic.WriteEntry(entry); err != nil {
		s.logger.Error("traffic log write failed", "request_id", entry.RequestID, "err", err)
	}
}
```

`writeError` 签名改为 `(w, format, status, message, rec)`，内部记录信封：

```go
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
```

`log` 方法加 `rec` 参数（取 request_id 并入 slog）：

```go
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
	if d.HasImage && !d.VisionSwitch {
		attrs = append(attrs, "image_detected", true)
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.logger.Info("proxy_request", attrs...)
}
```

- [ ] **Step 4: 实现 convert.go（签名扩展 + 非流式记录）**

`internal/server/convert.go`：

`relay` 签名加 `rec *trafficlog.Recorder`，`forward` 调用处传 `rec`；`forward` 签名加 `rec`。

`relay`（只改签名与 forward 调用）：

```go
func (s *Server) relay(w http.ResponseWriter, r *http.Request, cfg *config.Config, format string, body []byte, d route.Decision, rec *trafficlog.Recorder) error {
	...
	return s.forward(w, r, &up, outbound, format, d, rec)
	// 两处 forward 调用都要加 rec
}
```

`forward` 重构（整函数替换为带 rec 版本，含 ≥400 透传旁路）：

```go
func (s *Server) forward(w http.ResponseWriter, r *http.Request, up *config.UpstreamConfig, outbound []byte, inboundFormat string, d route.Decision, rec *trafficlog.Recorder) error {
	start := time.Now()
	if rec != nil {
		rec.SetOutbound(outbound)
	}
	resp, err := s.client.Do(r.Context(), up, outbound, false)
	if err != nil {
		if rec != nil {
			rec.SetError(err)
			rec.SetStatus(http.StatusBadGateway)
		}
		s.log(start, inboundFormat, d, *up, http.StatusBadGateway, false, err, rec)
		writeError(w, inboundFormat, http.StatusBadGateway, "upstream request failed", rec)
		return nil
	}
	defer resp.Body.Close()
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	// 上游非 2xx：错误体原样透传，同时旁路记录（C2）
	if resp.StatusCode >= 400 {
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		src := io.Reader(resp.Body)
		if rec != nil {
			src = io.TeeReader(resp.Body, rec.UpstreamWriter())
		}
		_, _ = io.Copy(w, src)
		if rec != nil {
			rec.SetError(fmt.Errorf("upstream status %d", resp.StatusCode))
			rec.SetStatus(resp.StatusCode)
		}
		s.log(start, inboundFormat, d, *up, resp.StatusCode, isStream, nil, rec)
		return nil
	}
	// 同格式：透传（头部原样复制；upstream 即 outbound，共用旁路）
	if inboundFormat == up.Format {
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		src := io.Reader(resp.Body)
		if rec != nil {
			src = io.TeeReader(resp.Body, rec.UpstreamWriter())
		}
		var copyErr error
		if isStream {
			copyErr = s.copyStream(w, r, src)
		} else {
			_, copyErr = io.Copy(w, src)
		}
		if errors.Is(copyErr, io.EOF) {
			copyErr = nil
		}
		if rec != nil {
			rec.SetStatus(resp.StatusCode)
		}
		s.log(start, inboundFormat, d, *up, resp.StatusCode, isStream, copyErr, rec)
		return nil
	}
	// 交叉格式：转换响应
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.StatusCode)
		cerr := s.convertStream(r.Context(), w, resp.Body, inboundFormat, up.Format, up.Model, rec)
		if rec != nil {
			rec.SetStatus(resp.StatusCode)
		}
		s.log(start, inboundFormat, d, *up, resp.StatusCode, true, cerr, rec)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if rec != nil {
			rec.SetError(err)
		}
		return fmt.Errorf("%w: read upstream response: %v", errUpstreamResponse, err)
	}
	if len(data) > maxBody {
		if rec != nil {
			rec.SetError(fmt.Errorf("response exceeds %d bytes", maxBody))
		}
		return fmt.Errorf("%w: exceeds %d bytes", errResponseTooLarge, maxBody)
	}
	if rec != nil {
		rec.SetUpstreamResponse(data)
	}
	var out any
	if inboundFormat == "openai" {
		var cr convert.MessagesResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return fmt.Errorf("%w: parse claude response: %v", errUpstreamResponse, err)
		}
		o, err := convert.ClaudeResponseToOpenAI(&cr, "")
		if err != nil {
			return err
		}
		o.Created = time.Now().Unix()
		out = o
	} else {
		var or convert.ChatCompletionResponse
		if err := json.Unmarshal(data, &or); err != nil {
			return fmt.Errorf("%w: parse openai response: %v", errUpstreamResponse, err)
		}
		o, err := convert.OpenAIResponseToClaude(&or, "")
		if err != nil {
			return err
		}
		out = o
	}
	outData, err := json.Marshal(out)
	if err != nil {
		s.log(start, inboundFormat, d, *up, resp.StatusCode, false, err, rec)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if rec != nil {
		rec.SetOutboundResponse(outData)
		rec.SetStatus(resp.StatusCode)
	}
	_, _ = w.Write(outData)
	s.log(start, inboundFormat, d, *up, resp.StatusCode, false, nil, rec)
	return nil
}
```

`convertStream` 签名加 `rec`（本轮只改签名，流式旁路在 Task 7 实现）：

```go
func (s *Server) convertStream(ctx context.Context, w http.ResponseWriter, src io.Reader, inboundFormat, outboundFormat, model string, rec *trafficlog.Recorder) error {
```

`copyStream` 不改（旁路在 forward 的 src TeeReader 处）。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: PASS（含新测试；`SetTrafficLog` 已存在，旧测试零改动）

- [ ] **Step 6: 提交**

```bash
git add internal/server/
git commit -m "feat(server): 注入内容日志 recorder，记录非流式四段与上游错误透传"
```

---

## Task 7: 流式旁路记录

**Files:**
- Modify: `internal/server/convert.go`、`internal/server/convert_test.go`

- [ ] **Step 1: 写失败测试**

`internal/server/convert_test.go` 末尾追加：

```go
func TestTrafficLog_StreamingFullCapture(t *testing.T) {
	var buf bytes.Buffer
	tl := trafficlog.NewWithWriter(&buf)
	// 上游 OpenAI SSE → 客户端 Claude（交叉格式）
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\ndata: [DONE]\n\n"))
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
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var e map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &e); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e["stream"] != true {
		t.Fatalf("stream: %v", e["stream"])
	}
	if !strings.Contains(e["upstream_response"].(string), "data: {\"choices\"") || !strings.Contains(e["upstream_response"].(string), "[DONE]") {
		t.Fatalf("upstream stream incomplete: %v", e["upstream_response"])
	}
	if !strings.Contains(e["outbound_response"].(string), "message_stop") {
		t.Fatalf("outbound stream missing stop: %v", e["outbound_response"])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestTrafficLog_Streaming -v`
Expected: FAIL（流式未旁路，outbound_response 为空或上游段为空）

- [ ] **Step 3: 实现 convert.go 的 convertStream 旁路**

`convertStream` 函数体修改：`src` 用 TeeReader 旁路上游段；回写处改用 `ww = io.MultiWriter(w, rec.OutboundWriter())`（Flusher 仍用原 `w`）。

```go
func (s *Server) convertStream(ctx context.Context, w http.ResponseWriter, src io.Reader, inboundFormat, outboundFormat, model string, rec *trafficlog.Recorder) error {
	src = newTimeoutReader(ctx, src, streamIdleTimeout)
	if rec != nil {
		src = io.TeeReader(src, rec.UpstreamWriter())
	}
	ww := w
	if rec != nil {
		ww = io.MultiWriter(w, rec.OutboundWriter())
	}
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
				_, _ = ww.Write(o)
			}
			if fl != nil {
				fl.Flush()
			}
			return nil
		}, func() error {
			if !c.Stopped() {
				return fmt.Errorf("truncated claude stream: EOF without message_stop")
			}
			for _, o := range c.Close() {
				_, _ = ww.Write(o)
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
	doneSeen := false
	for {
		data, done, err := scanner.Next(src)
		if done {
			doneSeen = true
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				streamErr = fmt.Errorf("truncated openai stream: EOF without [DONE]")
			} else {
				streamErr = err
			}
			break
		}
		outs, err := c.Write(data)
		if err != nil {
			streamErr = err
			break
		}
		fl, _ := w.(http.Flusher)
		for _, o := range outs {
			_, _ = ww.Write(o)
		}
		if fl != nil {
			fl.Flush()
		}
	}
	if doneSeen {
		for _, o := range c.Finish() {
			_, _ = ww.Write(o)
		}
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
	}
	return streamErr
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/server/convert.go internal/server/convert_test.go
git commit -m "feat(server): 流式响应旁路记录（TeeReader/MultiWriter 捕获完整 SSE）"
```

---

## Task 8: main.go 默认配置路径 + logging 初始化

**Files:**
- Modify: `cmd/prism-proxy/main.go`
- Create: `cmd/prism-proxy/main_test.go`

- [ ] **Step 1: 写失败测试**

`cmd/prism-proxy/main_test.go`：

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigPath_Explicit(t *testing.T) {
	got, err := resolveConfigPath("/tmp/x.yaml", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/x.yaml" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveConfigPath_Default(t *testing.T) {
	got, err := resolveConfigPath("~/.prism-proxy/settings.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".prism-proxy", "settings.yaml")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/prism-proxy/ -run TestResolveConfigPath -v`
Expected: FAIL（`resolveConfigPath` 未定义）

- [ ] **Step 3: 实现**

`cmd/prism-proxy/main.go`：

import 追加 `io/fs`、`path/filepath`、`prism-proxy/internal/trafficlog`。

追加：

```go
// defaultConfigFlag 为 --config 标志的默认字面值（help 展示用）。
const defaultConfigFlag = "~/.prism-proxy/settings.yaml"

// resolveConfigPath 解析配置路径：显式指定则原样返回；
// 缺省时展开 ~/.prism-proxy/settings.yaml 为绝对路径。
func resolveConfigPath(flagValue string, changed bool) (string, error) {
	if changed {
		return flagValue, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".prism-proxy", "settings.yaml"), nil
}
```

`serveCmd` 的 `RunE` 开头与配置加载改造：

```go
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := resolveConfigPath(configPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			watcher, err := config.NewWatcher(configPath)
			if err != nil {
				if !cmd.Flags().Changed("config") && errors.Is(err, fs.ErrNotExist) {
					// 缺省路径不存在：提示默认路径与旧默认迁移（review I3）
					return fmt.Errorf("config not found: %s; default config path is ~/.prism-proxy/settings.yaml; if you relied on the old default ./prism-proxy.yaml, pass --config prism-proxy.yaml explicitly", configPath)
				}
				return fmt.Errorf("invalid config (fail fast): %w", err)
			}
			defer watcher.Close()
			// 内容日志：启动时 enabled 则构造 TrafficLog（dir/max_files 变更需重启）
			var traffic *trafficlog.TrafficLog
			if cfg := watcher.Get(); cfg.Logging.Enabled {
				traffic, err = trafficlog.New(cfg.Logging.Dir, cfg.Logging.MaxFiles)
				if err != nil {
					return fmt.Errorf("init traffic log: %w", err)
				}
				defer traffic.Close()
			}
			srv := server.New(watcher, upstream.NewClient(), slog.Default())
			srv.SetTrafficLog(traffic)
			logger := slog.Default()
			logger.Info("prism-proxy listening", "addr", watcher.Get().Server.Listen, "config", configPath)
			...
```

注意 `configPath` 变量被 `resolveConfigPath(configPath, ...)` 同名遮蔽——使用另一个变量名避免：`path, err := resolveConfigPath(configPath, cmd.Flags().Changed("config"))`，后续用 `path`。

标志注册改为：

```go
	cmd.Flags().StringVar(&configPath, "config", defaultConfigFlag, "path to config file")
```

`main.go` 现有 import 已有 `errors`（`errors.Is(err, http.ErrServerClosed)`），`os` 已有。补 `io/fs`、`path/filepath`、`trafficlog`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/prism-proxy/ -v && go build ./...`
Expected: PASS + 构建成功

- [ ] **Step 5: 提交**

```bash
git add cmd/prism-proxy/
git commit -m "feat(cli): 缺省配置路径改为 ~/.prism-proxy/settings.yaml，启用内容日志初始化"
```

---

## Task 9: README + prism-proxy.yaml.example 更新

**Files:**
- Modify: `prism-proxy.yaml.example`、`README.md`

- [ ] **Step 1: 更新 example**

`prism-proxy.yaml.example` 顶部注释更新为：

```yaml
# prism-proxy 配置示例（复制为 ~/.prism-proxy/settings.yaml 后按需修改）
# 启动: prism-proxy serve           # 缺省读取 ~/.prism-proxy/settings.yaml
#       prism-proxy serve --config prism-proxy.yaml   # 显式指定
```

`server` 段后新增：

```yaml
logging:
  enabled: false                 # 内容日志开关；true 时记录每请求完整四段内容
  dir: ~/.prism-proxy/logs       # 日志目录；默认 ~/.prism-proxy/logs（~ 会展开）
  max_files: 3                   # 轮转保留旧文件数（traffic.log.1, .2, ...）；0 = 不删除
                                 # 轮转阈值 lumberjack 默认 100MB；dir/max_files 变更需重启
```

- [ ] **Step 2: 更新 README**

`README.md` 修改点：

1. **功能概述** 增补两条：
   - **默认配置路径**：`serve` 缺省读取 `~/.prism-proxy/settings.yaml`，不存在则启动失败（旧默认 `./prism-proxy.yaml` 请显式 `--config`）。
   - **内容日志（traffic log）**：`logging.enabled: true` 时，每请求一条 JSONL 记录客户端请求、出站请求、上游响应、出站响应四段完整内容（`api_key`/`key` 字段脱敏），写入 `~/.prism-proxy/logs/traffic.log`，100MB 轮转保留 `max_files` 个旧文件。

2. **命令行** 段更新：

```bash
prism-proxy version              # 打印版本
prism-proxy serve                # 启动代理（缺省配置 ~/.prism-proxy/settings.yaml）
prism-proxy serve --config prism-proxy.yaml   # 指定配置
```

3. **配置说明** 段增补 `logging` 段说明（enabled/dir/max_files；`enabled` 支持热切换，`dir`/`max_files` 需重启；`~` 展开）。

4. **热加载** 段注明 `logging.enabled` 热切换语义。

5. **日志字段** 段后新增「内容日志（traffic log）」小节：traffic.log JSONL 字段表（ts/request_id/inbound/upstream/outbound/model/stream/status/duration_ms/inbound_body/outbound_body/upstream_response/outbound_response/error）+ 每请求 `X-Request-Id` 响应头 + 脱敏说明。

6. **非目标** 段更新：去掉"无日志采样/审计持久化"，改为注明内容日志仅 `logging.enabled` 开启，`dir`/`max_files` 不支持热更新。

- [ ] **Step 3: 验证文档引用一致性**

Run: `grep -n "prism-proxy.yaml" README.md | head -20`
Expected: 显式传参示例保留，默认路径描述已更新

- [ ] **Step 4: 提交**

```bash
git add README.md prism-proxy.yaml.example
git commit -m "docs: 更新 README 与配置示例（默认路径、logging 段、内容日志字段说明）"
```

---

## Task 10: 全量验证

- [ ] **Step 1: 全量测试**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: 全部 PASS，构建与 vet 无错误

- [ ] **Step 2: 冒烟验证（mock 上游 + 内容日志）**

Run:

```bash
go build -o /tmp/prism-proxy ./cmd/prism-proxy
mkdir -p /tmp/pp && cat > /tmp/pp/settings.yaml <<'EOF'
server:
  listen: ":18787"
logging:
  enabled: true
  dir: /tmp/pp/logs
main:
  baseurl: "http://127.0.0.1:19999/v1"
  api_key: sk
  format: openai
  model: gpt-4o
EOF
```

另开终端起一个 mock 上游（返回 `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`），然后：

```bash
/tmp/prism-proxy serve --config /tmp/pp/settings.yaml &
curl -s http://127.0.0.1:18787/v1/chat/completions -H "Content-Type: application/json" -d '{"messages":[{"role":"user","content":"hello"}]}'
cat /tmp/pp/logs/traffic.log
```

Expected: 日志目录生成 traffic.log，含一条 JSONL（inbound_body 含 "hello"、upstream_response 含 "hi"、status 200、request_id 与响应头 X-Request-Id 一致）。

缺省路径冒烟（无 --config）：

```bash
HOME=/tmp/pp /tmp/prism-proxy serve
```

Expected: 报错 `config not found: /tmp/pp/.prism-proxy/settings.yaml; ... pass --config prism-proxy.yaml explicitly`

- [ ] **Step 3: 提交（如冒烟发现修复）**

```bash
git add -A
git commit -m "fix: 冒烟验证修复"
```

---

## 自审记录

- **Spec 覆盖**：默认路径（Task 8）、logging 配置段（Task 1）、TrafficLog 轮转（Task 4）、Redact 四段统一（Task 2/5）、segBuffer 超限落盘（Task 3）、流式旁路（Task 7）、上游 ≥400 透传（Task 6）、本地 writeError 信封（Task 6）、`X-Request-Id` 与 slog 关联（Task 6）、入站 413 只记 error（Task 6 `rec.SetError(errResponseTooLarge)` 分支）、热切换 enabled（Task 6 每请求 `cfg.Logging.Enabled` 判断）、dir/max_files 需重启（Task 8 启动时构造）、兼容错误提示（Task 8）、README/example（Task 9）。全部覆盖。
- **占位符**：无 TBD/TODO，所有步骤含完整代码与命令。
- **类型一致性**：`SetTrafficLog`、`NewRecorder(dir, requestID, inbound)`、`Recorder.Entry(stream, duration)`、`writeError(..., rec)`、`relay(..., rec)`、`forward(..., rec)`、`convertStream(..., rec)`、`log(..., rec)` 在各任务中签名一致；`rec.SetOutboundResponse` 在 writeError 与 forward 中一致；`errResponseTooLarge` 复用于入站 413 与上游响应超限（语义均为"响应太大"）。

**注**：入站 413 与上游响应超限共用 `errResponseTooLarge` 作为 recorder 的 error 文案（均为 "upstream response too large"）。如需区分可后续调整，不影响本计划功能。
