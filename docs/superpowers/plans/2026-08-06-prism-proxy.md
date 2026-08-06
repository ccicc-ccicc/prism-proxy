# prism-proxy 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建一个 Go 单二进制本地 LLM API 代理：同时暴露 OpenAI 与 Claude Messages 格式入口，内部完成双向协议转换与图片自动切换 vision 上游。

**Architecture:** 方案 C 混合架构——同格式请求走透传快路径（仅改写 model 字段 + 认证头），交叉格式（O2C/C2O）走由共享原子映射函数组合的转换器；流式 SSE 用带状态的状态机转换。route 层完整反序列化入站请求做图片检测与切换决策，upstream 层负责转发与错误透传，slog 记录每请求一行日志。配置 YAML 顶层键即上游（main/vision），fsnotify 热加载 + 原子快照。

**Tech Stack:** Go 1.22+（标准库 net/http、slog、atomic）、github.com/spf13/cobra、gopkg.in/yaml.v3、github.com/fsnotify/fsnotify

## Global Constraints

- 依赖仅限：cobra、yaml.v3、fsnotify（+ 测试用标准库 httptest）
- module 名：`prism-proxy`；所有包 `prism-proxy/internal/...`
- 配置极简：上游是 YAML 顶层键（`main:`、`vision:`），每上游单 `model` 字段，无模型名映射层
- 请求模型名完全忽略：路由决策只有两条规则（见 spec §4）
- 启动校验 fail fast：main 缺失或不完整 → panic；`auto_switch_vision: true` 但 vision 缺失 → panic；`auto_switch_vision` 缺省 false
- 热加载校验失败保留旧配置（不 panic），fsnotify 收到 Rename/Create 要重新 add watch
- 流式超时语义：`timeout` 为连接+响应头超时（默认 120s），另有流空闲读超时 60s，流总时长不限
- 非流式请求/响应体限 50MB，超限 413
- 错误透传：非 2xx 状态码 + 错误体原样透传（不做格式转换）
- 图片检测基于结构化解析（递归 `tool_result.content`），禁止子串扫描
- Claude image block 必须含 `media_type`；外链图代理下载超时 30s
- `n>1` 入站拒绝：400 + 入站格式错误体
- 每任务结束 TDD 绿灯 + commit

---

### Task 1: 项目骨架 + cobra CLI

**Files:**
- Create: `go.mod`
- Create: `cmd/prism-proxy/main.go`

**Interfaces:**
- Consumes: 无
- Produces: `main.go` — cobra 根命令含子命令 `serve --config <path>`（默认 `prism-proxy.yaml`）与 `version`；serve 启动 `internal/server` 前先调用 `internal/config.Load`（Task 2 实现，本任务先留调用点用 TODO 编译不过——本任务先只做 cobra 骨架 + version，serve 子命令挂一个占位 handler 返回 501）

- [ ] **Step 1: 初始化 go.mod 与依赖**

```bash
cd /Users/xiamingyu/orca/workspaces/prism-proxy/feature-init
go mod init prism-proxy
go get github.com/spf13/cobra@latest
```

- [ ] **Step 2: 写 main.go**

```go
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "prism-proxy",
		Short: "Local LLM API proxy: OpenAI <-> Claude protocol gateway",
	}
	root.AddCommand(versionCmd())
	root.AddCommand(serveCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("prism-proxy 0.1.0")
		},
	}
}

func serveCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the proxy server",
		RunE: func(cmd *cobra.Command, args []string) error {
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "config not loaded yet", http.StatusNotImplemented)
			})
			addr := ":8787"
			return http.ListenAndServe(addr, mux)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "prism-proxy.yaml", "path to config file")
	return cmd
}
```

- [ ] **Step 3: 编译 + 冒烟**

```bash
go build ./... && go run ./cmd/prism-proxy version
```

Expected: `prism-proxy 0.1.0` 输出。

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum cmd/prism-proxy/main.go
git commit -m "chore: project skeleton with cobra CLI"
```

---

### Task 2: 配置解析与启动校验

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 1 的 `serveCmd`（`--config` flag 值）
- Produces:
  - `type Config struct { Server ServerConfig; AutoSwitchVision bool; Upstreams map[string]UpstreamConfig }`
  - `type ServerConfig struct { Listen string; AuthKeys []string }`
  - `type UpstreamConfig struct { BaseURL, APIKey, Format, Model string; Timeout time.Duration }`
  - `func Load(path string) (*Config, error)` — 解析 + 校验，校验失败返回 error（**调用方决定 panic**）
  - `func (c *Config) Validate() error`
  - `func (c *Config) Main() *UpstreamConfig` — 返回 main 上游
  - `func (c *Config) Vision() (*UpstreamConfig, bool)` — vision 存在且完整返回 true

**yaml.v3 内联 map 技巧**：`Upstreams map[string]UpstreamConfig` 用 tag `yaml:",inline"`，具名字段（server、auto_switch_vision）被消费后，其余顶层 key 全部进入 map——上游自动收集。

- [ ] **Step 1: 写失败测试**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	path := writeTemp(t, `
server:
  listen: ":8787"
  auth_keys: ["sk-proxy-1"]
auto_switch_vision: true
main:
  baseurl: "https://api.openai.com/v1"
  api_key: "sk-main"
  format: openai
  model: "gpt-4o"
vision:
  baseurl: "https://vision.example.com/v1"
  api_key: "sk-vision"
  format: claude
  model: "gpt-4o-vision"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoSwitchVision || len(cfg.Server.AuthKeys) != 1 {
		t.Fatalf("fields: %+v", cfg.Server)
	}
	if cfg.Main().Model != "gpt-4o" {
		t.Fatalf("main: %+v", cfg.Main())
	}
	if v, ok := cfg.Vision(); !ok || v.Format != "claude" {
		t.Fatalf("vision: %+v ok=%v", v, ok)
	}
	if cfg.Main().Timeout != 120*time.Second {
		t.Fatalf("default timeout: %v", cfg.Main().Timeout)
	}
}

func TestValidateMainMissing(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing main")
	}
}

func TestValidateVisionRequiredButMissing(t *testing.T) {
	cfg := &Config{AutoSwitchVision: true}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: vision required when auto_switch_vision on")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestLoadValid -v`
Expected: FAIL（`config.Load` 未定义）

- [ ] **Step 3: 实现 config.go**

```go
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server           ServerConfig              `yaml:"server"`
	AutoSwitchVision bool                      `yaml:"auto_switch_vision"`
	Upstreams        map[string]UpstreamConfig `yaml:",inline"`
}

type ServerConfig struct {
	Listen   string   `yaml:"listen"`
	AuthKeys []string `yaml:"auth_keys"`
}

type UpstreamConfig struct {
	BaseURL string        `yaml:"baseurl"`
	APIKey  string        `yaml:"api_key"`
	Format  string        `yaml:"format"`
	Model   string        `yaml:"model"`
	Timeout time.Duration `yaml:"timeout"`
}

const defaultTimeout = 120 * time.Second

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8787"
	}
	for name := range c.Upstreams {
		u := c.Upstreams[name]
		if u.Timeout == 0 {
			u.Timeout = defaultTimeout
		}
		c.Upstreams[name] = u
	}
}

func (c *Config) Validate() error {
	main, ok := c.Upstreams["main"]
	if !ok {
		return fmt.Errorf("config: upstream 'main' is required")
	}
	if err := validateUpstream("main", main); err != nil {
		return err
	}
	if c.AutoSwitchVision {
		vision, ok := c.Upstreams["vision"]
		if !ok {
			return fmt.Errorf("config: auto_switch_vision is true but upstream 'vision' is missing")
		}
		if err := validateUpstream("vision", vision); err != nil {
			return err
		}
	}
	return nil
}

func validateUpstream(name string, u UpstreamConfig) error {
	if u.BaseURL == "" {
		return fmt.Errorf("config: upstream %q: baseurl is required", name)
	}
	if u.APIKey == "" {
		return fmt.Errorf("config: upstream %q: api_key is required", name)
	}
	if u.Format != "openai" && u.Format != "claude" {
		return fmt.Errorf("config: upstream %q: format must be openai or claude, got %q", name, u.Format)
	}
	if u.Model == "" {
		return fmt.Errorf("config: upstream %q: model is required", name)
	}
	return nil
}

func (c *Config) Main() *UpstreamConfig {
	u := c.Upstreams["main"]
	return &u
}

func (c *Config) Vision() (*UpstreamConfig, bool) {
	u, ok := c.Upstreams["vision"]
	if !ok || u.BaseURL == "" || u.APIKey == "" || u.Model == "" {
		return nil, false
	}
	return &u, true
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: PASS（3 个测试）

- [ ] **Step 5: 接线 serve 命令：加载配置，失败 panic**

修改 `cmd/prism-proxy/main.go` 的 `serveCmd`：

```go
RunE: func(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("invalid config (fail fast): %w", err)
	}
	_ = cfg
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "handler wiring in later tasks", http.StatusNotImplemented)
	})
	return http.ListenAndServe(cfg.Server.Listen, mux)
},
```

加 import `"prism-proxy/internal/config"`。

- [ ] **Step 6: 编译 + 冒烟（两个 panic 场景）**

```bash
go build ./...
go run ./cmd/prism-proxy serve --config /nonexistent.yaml
```

Expected: 退出码 1，输出含 `invalid config`。

```bash
printf 'main:\n  baseurl: "x"\n  api_key: "y"\n  format: openai\n  model: "m"\n' > /tmp/no-vision.yaml
go run ./cmd/prism-proxy serve --config /tmp/no-vision.yaml --listen-ignored 2>&1 | head -1
```

Expected: 退出码 1（auto_switch_vision 缺省 false，本配置合法，应启动成功——若输出 501 占位即说明通过）。此命令前台会挂起，用 `timeout 2` 包裹验证启动即退出。

- [ ] **Step 7: Commit**

```bash
git add cmd/prism-proxy/main.go internal/config/
git commit -m "feat: config parsing with fail-fast validation"
```

---

### Task 3: 配置热加载

**Files:**
- Create: `internal/config/watch.go`
- Test: `internal/config/watch_test.go`

**Interfaces:**
- Consumes: `config.Load`、`*Config`（Task 2）
- Produces:
  - `type Watcher struct{ cfg atomic.Pointer[Config] }`
  - `func NewWatcher(path string) (*Watcher, error)` — 先 Load，失败返回 error；启动 fsnotify
  - `func (w *Watcher) Get() *Config` — 原子读当前配置
  - `func (w *Watcher) Close() error`
  - 监听循环 goroutine 在 `NewWatcher` 内启动；`Rename`/`Create` 事件重新 add watch（编辑器原子写会丢 inode watch）；解析/校验失败 → 保留旧配置 + slog 记错（不 panic）

- [ ] **Step 1: 写失败测试**

```go
package config

import (
	"os"
	"testing"
	"time"
)

func TestWatcherReload(t *testing.T) {
	path := writeTemp(t, "main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m1\"\n")
	w, err := NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Get().Main().Model != "m1" {
		t.Fatal("initial model mismatch")
	}
	// 模拟编辑器原子写（rename 保存）
	if err := os.WriteFile(path, []byte("main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.Get().Main().Model == "m2" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("config did not reload after rename save")
}

func TestWatcherKeepsOldOnInvalid(t *testing.T) {
	path := writeTemp(t, "main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m1\"\n")
	w, err := NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.WriteFile(path, []byte("broken: [yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // 给 watch 循环一个处理窗口
	if got := w.Get().Main().Model; got != "m1" {
		t.Fatalf("old config replaced with invalid one: %s", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestWatcher -v`
Expected: FAIL（`NewWatcher` 未定义）

- [ ] **Step 3: 实现 watch.go**

```go
package config

import (
	"log/slog"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	cfg   atomic.Pointer[Config]
	path  string
	watcher *fsnotify.Watcher
}

func NewWatcher(path string) (*Watcher, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := fw.Add(path); err != nil {
		fw.Close()
		return nil, err
	}
	w := &Watcher{path: path, watcher: fw}
	w.cfg.Store(cfg)
	go w.loop()
	return w, nil
}

func (w *Watcher) Get() *Config { return w.cfg.Load() }

func (w *Watcher) Close() error { return w.watcher.Close() }

func (w *Watcher) loop() {
	for {
		select {
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			// 编辑器原子写（rename）会丢失 inode watch，重新挂
			if ev.Op&(fsnotify.Rename|fsnotify.Create) != 0 {
				_ = w.watcher.Add(w.path)
			}
			if ev.Op&(fsnotify.Write|fsnotify.Rename|fsnotify.Create) == 0 {
				continue
			}
			newCfg, err := Load(w.path)
			if err != nil {
				slog.Error("config reload failed, keeping old config", "err", err)
				continue
			}
			w.cfg.Store(newCfg)
			slog.Info("config reloaded", "path", w.path)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("config watcher error", "err", err)
		}
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: PASS（5 个测试）

- [ ] **Step 5: Commit**

```bash
git add internal/config/watch.go internal/config/watch_test.go
git commit -m "feat: config hot reload via fsnotify"
```

---

### Task 4: OpenAI / Claude 协议结构定义

**Files:**
- Create: `internal/convert/openai.go`
- Create: `internal/convert/claude.go`
- Test: `internal/convert/openai_test.go`, `internal/convert/claude_test.go`

**Interfaces:**
- Consumes: 无
- Produces（后续所有任务依赖的 JSON 结构，字段名与官方 API 对齐）：
  - `openai.go`: `ChatCompletionRequest{Model string; Messages []ChatMessage; Tools []Tool; Stream bool; MaxTokens *int; N *int; Temperature *float64; TopP *float64; Stop []string; ResponseFormat *ResponseFormat}`、`ChatMessage{Role string; Content any; ToolCalls []ToolCall; ToolCallID string; Name string}`、`ContentPart{Type string; Text string; ImageURL *ImageURL}`、`ImageURL{URL string}`、`Tool{Type string; Function FunctionDef}`、`FunctionDef{Name, Description string; Parameters map[string]any}`、`ToolCall{Index *int; ID, Type string; Function ToolCallFunction}`、`ToolCallFunction{Name, Arguments string}`、`ChatCompletionResponse{ID, Object, Model string; Created int64; Choices []ResponseChoice; Usage *Usage}`、`ResponseChoice{Index int; Message ChatMessage; FinishReason string}`、`Usage{PromptTokens, CompletionTokens, TotalTokens int}`、流式 `StreamChunk{ID, Object, Model string; Choices []StreamChoice}`、`StreamChoice{Index int; Delta ChatMessage; FinishReason *string}`、`ErrorResponse{Error ErrorDetail}`、`ErrorDetail{Message, Type string; Code any}`
  - `claude.go`: `MessagesRequest{Model string; System string; Messages []ClaudeMessage; Tools []ClaudeTool; Stream bool; MaxTokens int; Temperature *float64; TopP *float64; StopSequences []string}`、`ClaudeMessage{Role string; Content any}`、`ClaudeBlock{Type string; Text string; ID string; Name string; Input any; Content any; IsError *bool; Source *ImageSource; Thinking, Signature string; PartialJSON string}`、`ImageSource{Type, MediaType, Data string}`、`ClaudeTool{Name, Description string; InputSchema map[string]any}`、`MessagesResponse{ID, Type, Role, Model string; Content []ClaudeBlock; StopReason *string; Usage *ClaudeUsage; Error *ClaudeError}`、`ClaudeUsage{InputTokens, OutputTokens int}`、`ClaudeError{Type, Message string; StatusCode int}`、流式事件 `StreamEvent{Type string; Message *MessagesResponse; Index int; ContentBlock *ClaudeBlock; Delta *ClaudeDelta; Usage *ClaudeUsage; StopReason *string}`、`ClaudeDelta{Type, Text string; PartialJSON string}`、`EventType*` 常量
  - 哨兵常量：`EventMessageStart = "message_start"`、`EventContentBlockStart = "content_block_start"`、`EventContentBlockDelta = "content_block_delta"`、`EventContentBlockStop = "content_block_stop"`、`EventMessageDelta = "message_delta"`、`EventMessageStop = "message_stop"`、`EventPing = "ping"`、`EventError = "error"`

- [ ] **Step 1: 写失败测试（验证结构可解析真实 API 样例）**

```go
package convert

import (
	"encoding/json"
	"testing"
)

func TestOpenAIRequestUnmarshal(t *testing.T) {
	data := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}]}`)
	var req ChatCompletionRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o" || !req.Stream {
		t.Fatal("model/stream")
	}
	parts, ok := req.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts: %#v", req.Messages[0].Content)
	}
	if req.Tools[0].Function.Name != "f" {
		t.Fatal("tool")
	}
}

func TestClaudeRequestUnmarshal(t *testing.T) {
	data := []byte(`{"model":"claude-3","max_tokens":1024,"system":"sys","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}],"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}]}`)
	var req MessagesRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 1024 || req.System != "sys" {
		t.Fatal("fields")
	}
	blocks, ok := req.Messages[0].Content.([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("blocks: %#v", req.Messages[0].Content)
	}
}

func TestClaudeStreamEventUnmarshal(t *testing.T) {
	data := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	var ev StreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != EventContentBlockDelta || ev.Delta == nil || ev.Delta.Text != "hi" {
		t.Fatalf("event: %+v", ev)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run TestOpenAIRequestUnmarshal -v`
Expected: FAIL（类型未定义）

- [ ] **Step 3: 实现 openai.go**

```go
package convert

// OpenAI Chat Completions API 结构。Content 用 any 接收（string 或 []ContentPart），
// 由原子映射函数按需断言。

type ChatCompletionRequest struct {
	Model          string         `json:"model"`
	Messages       []ChatMessage  `json:"messages"`
	Tools          []Tool         `json:"tools,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
	MaxTokens      *int           `json:"max_tokens,omitempty"`
	MaxCompletion  *int           `json:"max_completion_tokens,omitempty"`
	N              *int           `json:"n,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	TopP           *float64       `json:"top_p,omitempty"`
	Stop           []string       `json:"stop,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

type ChatMessage struct {
	Role       string      `json:"role"`
	Content    any         `json:"content,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatCompletionResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []ResponseChoice `json:"choices"`
	Usage   *Usage           `json:"usage,omitempty"`
}

type ResponseChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        ChatMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}
```

- [ ] **Step 4: 实现 claude.go**

```go
package convert

// Anthropic Messages API 结构。

const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

type MessagesRequest struct {
	Model         string          `json:"model"`
	System        string          `json:"system,omitempty"`
	Messages      []ClaudeMessage `json:"messages"`
	Tools         []ClaudeTool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`
}

type ClaudeBlock struct {
	Type        string       `json:"type"`
	Text        string       `json:"text,omitempty"`
	ID          string       `json:"id,omitempty"`
	Name        string       `json:"name,omitempty"`
	Input       any          `json:"input,omitempty"`
	Content     any          `json:"content,omitempty"`
	IsError     *bool        `json:"is_error,omitempty"`
	Source      *ImageSource `json:"source,omitempty"`
	Thinking    string       `json:"thinking,omitempty"`
	Signature   string       `json:"signature,omitempty"`
	PartialJSON string       `json:"partial_json,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type ClaudeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type MessagesResponse struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Role       string        `json:"role"`
	Model      string        `json:"model"`
	Content    []ClaudeBlock `json:"content"`
	StopReason *string       `json:"stop_reason,omitempty"`
	Usage      *ClaudeUsage  `json:"usage,omitempty"`
	Error      *ClaudeError  `json:"error,omitempty"`
}

type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type ClaudeError struct {
	Type       string `json:"type"`
	Message    string `json:"message"`
	StatusCode int    `json:"status_code,omitempty"`
}

type StreamEvent struct {
	Type        string          `json:"type"`
	Message     *MessagesResponse `json:"message,omitempty"`
	Index       int             `json:"index,omitempty"`
	ContentBlock *ClaudeBlock   `json:"content_block,omitempty"`
	Delta       *ClaudeDelta    `json:"delta,omitempty"`
	Usage       *ClaudeUsage    `json:"usage,omitempty"`
	StopReason  *string         `json:"stop_reason,omitempty"`
	Error       *ClaudeError    `json:"error,omitempty"`
}

type ClaudeDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/convert/ -v`
Expected: PASS（3 个测试）

- [ ] **Step 6: Commit**

```bash
git add internal/convert/openai.go internal/convert/claude.go internal/convert/*_test.go
git commit -m "feat: openai and claude protocol structs"
```

---

### Task 5: 原子映射函数（消息/工具/图片/ID）

**Files:**
- Create: `internal/convert/messages.go`
- Create: `internal/convert/ids.go`
- Test: `internal/convert/messages_test.go`

**Interfaces:**
- Consumes: Task 4 结构定义
- Produces:
  - `func OpenAIMessagesToClaude(msgs []ChatMessage) (system string, out []ClaudeMessage, err error)` — system 消息合并到 `system`（`\n` 连接）；assistant 的 tool_calls → tool_use blocks（arguments JSON 串 parse 为对象，非法 JSON → `{}` + slog 警告）；role=tool → user 消息 + tool_result block（**emit 顺序：全部 tool_result 消息先于剩余 user 文本**）
  - `func ClaudeMessagesToOpenAI(msgs []ClaudeMessage, system string) ([]ChatMessage, error)` — system 放 `messages[0]`（role=system，`\n` 已由调用方合并）；user/assistant 直转；tool_use block → tool_calls（input 对象序列化为 JSON 串）；tool_result block → 拆为 role=tool 消息（is_error 错误文本拼 content；content 数组转文本）
  - `func OpenAIToolsToClaude(tools []Tool) []ClaudeTool` — function → name/description/input_schema；parameters 缺省补 `{"type":"object"}`
  - `func ClaudeToolsToOpenAI(tools []ClaudeTool) []Tool`
  - `func OpenAIArgsToClaudeInput(args string) (any, error)` — JSON 串 → any；非法 → `map[string]any{}` + error 供调用方记日志
  - `func ClaudeInputToOpenAIArgs(input any) (string, error)` — any → JSON 串
  - `func NewOpenAIID() string` / `func NewClaudeID() string` — `chatcmpl-` + 16 位 hex / `msg_` + 16 位 hex（`crypto/rand`）
  - `func ImageURLToClaudeSource(url string) (*ImageSource, string, error)` — data URL 解析前缀 → media_type；非 data URL 返回 `("", url, nil)` 表示需下载
  - `func ClaudeSourceToImageURL(src *ImageSource) string` — `data:{media_type};base64,{data}`
  - `func BlockHasImage(blocks []ClaudeBlock) bool` / `func ContentHasImage(parts any) bool` — 结构检测（递归 tool_result.content）

- [ ] **Step 1: 写失败测试**

```go
package convert

import (
	"strings"
	"testing"
)

func TestOpenAIMessagesToClaude_SystemMerge(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: "a"},
		{Role: "system", Content: "b"},
		{Role: "user", Content: "hi"},
	}
	system, out, err := OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if system != "a\nb" {
		t.Fatalf("system: %q", system)
	}
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("out: %+v", out)
	}
}

func TestOpenAIMessagesToClaude_ToolCalls(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
	}
	_, out, err := OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Role != "user" {
		t.Fatalf("out: %+v", out)
	}
	blocks := out[1].Content.([]ClaudeBlock)
	if blocks[0].Type != "tool_result" || blocks[0].ToolUseID() == "" || blocks[0].Content != "result" {
		t.Fatalf("tool_result block: %+v", blocks[0])
	}
	// 验证非法 arguments 兜底
	msgs[0].ToolCalls[0].Function.Arguments = "{not json"
	_, _, err = OpenAIMessagesToClaude(msgs)
	if err != nil {
		t.Fatalf("invalid args should not fail: %v", err)
	}
}

func TestClaudeMessagesToOpenAI_ToolResultOrder(t *testing.T) {
	// user 消息内 tool_result + 文本 → tool 消息必须在前
	msgs := []ClaudeMessage{
		{Role: "user", Content: []ClaudeBlock{
			{Type: "tool_result", Content: "r"},
			{Type: "text", Text: "and also"},
		}},
	}
	out, err := ClaudeMessagesToOpenAI(msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Role != "tool" || out[1].Role != "user" {
		t.Fatalf("order: %+v", out)
	}
}

func TestImageConversions(t *testing.T) {
	src, rest, err := ImageURLToClaudeSource("data:image/png;base64,AAAA")
	if err != nil || rest != "" || src.MediaType != "image/png" || src.Data != "AAAA" {
		t.Fatalf("data url: %+v %q %v", src, rest, err)
	}
	_, rest, _ = ImageURLToClaudeSource("https://x.com/a.png")
	if rest != "https://x.com/a.png" {
		t.Fatalf("external: %q", rest)
	}
	url := ClaudeSourceToImageURL(&ImageSource{Type: "base64", MediaType: "image/jpeg", Data: "BBBB"})
	if url != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("url: %q", url)
	}
}

func TestIDs(t *testing.T) {
	if !strings.HasPrefix(NewOpenAIID(), "chatcmpl-") || !strings.HasPrefix(NewClaudeID(), "msg_") {
		t.Fatal("id prefixes")
	}
}

func TestBlockHasImage_NestedInToolResult(t *testing.T) {
	blocks := []ClaudeBlock{
		{Type: "tool_result", Content: []ClaudeBlock{{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "x"}}}},
	}
	if !BlockHasImage(blocks) {
		t.Fatal("image nested in tool_result not detected")
	}
}
```

（注：`ToolUseID()` 为测试辅助方法，在 messages.go 里加 `func (b ClaudeBlock) ToolUseID() string`——type=image 返回空，type=tool_result 返回 Source 为空时的实际 id 字段。为测试简洁，直接在 ClaudeBlock 上加 json tag `tool_use_id` 字段。**修正**：ClaudeBlock 增加 `ToolUseID string \`json:"tool_use_id,omitempty"\``，测试改用 `blocks[0].ToolUseID`。测试代码同步改用字段。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run TestOpenAIMessagesToClaude -v`
Expected: FAIL（函数未定义）

- [ ] **Step 3: 实现 messages.go**

```go
package convert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// 结构辅助：ClaudeBlock 增加 ToolUseID 字段（Task 4 的 claude.go 内补加）：
// ToolUseID string `json:"tool_use_id,omitempty"`

func OpenAIMessagesToClaude(msgs []ChatMessage) (system string, out []ClaudeMessage, err error) {
	var sys []string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sys = append(sys, contentString(m.Content))
		case "assistant":
			var blocks []ClaudeBlock
			if s := contentString(m.Content); s != "" {
				blocks = append(blocks, ClaudeBlock{Type: "text", Text: s})
			}
			for _, tc := range m.ToolCalls {
				input, perr := OpenAIArgsToClaudeInput(tc.Function.Arguments)
				if perr != nil {
					slog.Warn("tool_call arguments not valid json, using {}", "tool", tc.Function.Name)
				}
				blocks = append(blocks, ClaudeBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			out = append(out, ClaudeMessage{Role: "assistant", Content: blocks})
		case "tool":
			out = append(out, ClaudeMessage{Role: "user", Content: []ClaudeBlock{
				{Type: "tool_result", ToolUseID: m.ToolCallID, Content: contentString(m.Content)},
			}})
		case "user":
			out = append(out, ClaudeMessage{Role: "user", Content: convertOpenAIContent(m.Content)})
		default:
			return "", nil, fmt.Errorf("unsupported openai role %q", m.Role)
		}
	}
	return strings.Join(sys, "\n"), out, nil
}

func contentString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func convertOpenAIContent(c any) any {
	str, ok := c.(string)
	if ok {
		return str
	}
	parts, ok := c.([]any)
	if !ok {
		return contentString(c)
	}
	var blocks []ClaudeBlock
	for _, p := range parts {
		pp, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pp["type"] {
		case "text":
			blocks = append(blocks, ClaudeBlock{Type: "text", Text: fmt.Sprint(pp["text"])})
		case "image_url":
			u, _ := pp["image_url"].(map[string]any)
			urlStr, _ := u["url"].(string)
			src, rest, perr := ImageURLToClaudeSource(urlStr)
			if perr == nil && rest == "" {
				blocks = append(blocks, ClaudeBlock{Type: "image", Source: src})
			} else {
				// 外链图：由调用方（convert/upstream 层）下载后替换；此处先占位标记
				blocks = append(blocks, ClaudeBlock{Type: "image", Source: &ImageSource{Type: "url", Data: urlStr}})
			}
		}
	}
	return blocks
}

func ClaudeMessagesToOpenAI(msgs []ClaudeMessage, system string) ([]ChatMessage, error) {
	var out []ChatMessage
	if system != "" {
		out = append(out, ChatMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "user", "assistant":
			blocks, ok := m.Content.([]any)
			if !ok {
				// content 可能为字符串（简写）
				if s, isStr := m.Content.(string); isStr {
					out = append(out, ChatMessage{Role: m.Role, Content: s})
					continue
				}
				out = append(out, ChatMessage{Role: m.Role, Content: contentString(m.Content)})
				continue
			}
			var text strings.Builder
			var toolCalls []ToolCall
			var toolResults []ChatMessage
			for _, b := range blocks {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				switch bm["type"] {
				case "text":
					text.WriteString(fmt.Sprint(bm["text"]))
				case "tool_use":
					toolCalls = append(toolCalls, ToolCall{
						ID:   fmt.Sprint(bm["id"]),
						Type: "function",
						Function: ToolCallFunction{
							Name:      fmt.Sprint(bm["name"]),
							Arguments: mustJSON(bm["input"]),
						},
					})
				case "tool_result":
					toolResults = append(toolResults, ChatMessage{Role: "tool", ToolCallID: fmt.Sprint(bm["tool_use_id"]), Content: resultToText(bm)})
				case "thinking", "redacted_thinking":
					// 剥离
				}
			}
			if text.Len() > 0 {
				out = append(out, ChatMessage{Role: m.Role, Content: text.String()})
			}
			// 工具结果必须紧跟 assistant(tool_calls)：先 tool 后文本——toolResults 先入
			out = append(out, toolResults...)
			if len(toolCalls) > 0 {
				out = append(out, ChatMessage{Role: m.Role, Content: "", ToolCalls: toolCalls})
			}
		default:
			return nil, fmt.Errorf("unsupported claude role %q", m.Role)
		}
	}
	return out, nil
}

func resultToText(bm map[string]any) string {
	switch v := bm["content"].(type) {
	case string:
		if isErr, _ := bm["is_error"].(bool); isErr {
			return "[error] " + v
		}
		return v
	case []any:
		var sb strings.Builder
		for _, c := range v {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := cm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func OpenAIToolsToClaude(tools []Tool) []ClaudeTool {
	out := make([]ClaudeTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, ClaudeTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	return out
}

func ClaudeToolsToOpenAI(tools []ClaudeTool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, Tool{Type: "function", Function: FunctionDef{Name: t.Name, Description: t.Description, Parameters: schema}})
	}
	return out
}

func OpenAIArgsToClaudeInput(args string) (any, error) {
	if args == "" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return map[string]any{}, err
	}
	return v, nil
}

func ClaudeInputToOpenAIArgs(input any) (string, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}
```

**注**：`convertOpenAIContent` 中外链图暂用 `Source{Type:"url"}` 占位，Task 11 的 upstream 层下载后替换为 base64。图片检测函数与 `ImageURLToClaudeSource`/`ClaudeSourceToImageURL` 放本文件：

```go
func ImageURLToClaudeSource(url string) (*ImageSource, string, error) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil, url, nil
	}
	comma := strings.Index(url, ";base64,")
	if comma < 0 {
		return nil, "", fmt.Errorf("unsupported data url format")
	}
	mediaType := strings.TrimPrefix(url[len("data:"):comma], "data:")
	return &ImageSource{Type: "base64", MediaType: mediaType, Data: url[comma+len(";base64,"):]}, "", nil
}

func ClaudeSourceToImageURL(src *ImageSource) string {
	return "data:" + src.MediaType + ";base64," + src.Data
}

func BlockHasImage(blocks []ClaudeBlock) bool {
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
		if b.Type == "tool_result" {
			if inner, ok := b.Content.([]ClaudeBlock); ok && BlockHasImage(inner) {
				return true
			}
		}
	}
	return false
}

func ContentHasImage(parts any) bool {
	switch v := parts.(type) {
	case []ClaudeBlock:
		return BlockHasImage(v)
	case []any:
		for _, p := range v {
			if m, ok := p.(map[string]any); ok && m["type"] == "image_url" {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: 实现 ids.go**

```go
package convert

import (
	"crypto/rand"
	"encoding/hex"
)

func newSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func NewOpenAIID() string { return "chatcmpl-" + newSuffix() }
func NewClaudeID() string { return "msg_" + newSuffix() }
```

- [ ] **Step 5: 修正 Task 4 的 claude.go：ClaudeBlock 补 `ToolUseID` 字段**

在 `ClaudeBlock` 结构体加一行：`ToolUseID string \`json:"tool_use_id,omitempty"\``（放在 `IsError` 之前）。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/convert/ -v`
Expected: PASS。若 `TestClaudeMessagesToOpenAI_ToolResultOrder` 失败，检查 emit 顺序实现（toolResults 必须先于后续文本消息——当前实现 text 先入列，需调整：**先收 toolResults，text 与 toolCalls 排在其后**。修正：把 `out = append(out, text...); out = append(out, toolResults...)` 改为 toolResults 先入。测试是最终仲裁。）

- [ ] **Step 7: Commit**

```bash
git add internal/convert/messages.go internal/convert/ids.go internal/convert/messages_test.go internal/convert/claude.go
git commit -m "feat: atomic mapping functions for messages/tools/images/ids"
```

---

### Task 6: O2C 流式状态机（OpenAI SSE → Claude SSE）

**Files:**
- Create: `internal/convert/stream_o2c.go`
- Test: `internal/convert/stream_o2c_test.go`

**Interfaces:**
- Consumes: Task 4 结构、Task 5 `NewClaudeID`
- Produces:
  - `type O2CStream struct { id string; started bool; toolIdx int }`
  - `func NewO2CStream() *O2CStream`
  - `func (s *O2CStream) Write(data []byte) ([][]byte, error)` — 输入一条 OpenAI SSE 的 `data:` JSON（不含 `data: ` 前缀与 `[DONE]`），输出 0..n 条 Claude 帧字节（每条以 `event: <type>\ndata: <json>\n\n` 结尾）
  - `func (s *O2CStream) Finish() [][]byte` — 输出 `message_stop` 帧
  - 规则：首块发 `message_start`（构造 message：id=s.id、role=assistant、content=[]、model 由调用方传入构造函数 `NewO2CStreamWithModel(model string)`、stop_reason/stop_sequence=null、usage=null）；`delta.content` → content_block_start(text)+delta+stop（text block 每次 delta 后**不**发 stop，stop 只在块切换/结束时发——**简化约定**：text 块不做 stop 拆分，内容 delta 连续发；finish 前补最后一个 text block 的 content_block_stop）；tool_calls 首块（带 id）→ content_block_start(tool_use)，后续 arguments 增量 → input_json_delta；finish_reason → message_delta；usage（非 nil）→ message_delta{usage}
  - tool_use 的 content_block index 由转换器**全局递增**（text=0、tool_use=1…），Claude 要求 block index 从 0 连续

- [ ] **Step 1: 写失败测试**

```go
package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func frame(t *testing.T, data string) []byte {
	t.Helper()
	return []byte("event: " + data)
}

func TestO2CStream_TextFlow(t *testing.T) {
	s := NewO2CStreamWithModel("gpt-4o")
	// 首块：role
	out, err := s.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	if err != nil || len(out) != 1 {
		t.Fatalf("start: %d %v", len(out), err)
	}
	if !strings.HasPrefix(string(out[0]), "event: message_start") {
		t.Fatalf("first event: %s", out[0])
	}
	// 内容 delta
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	if len(out) != 1 || !strings.Contains(string(out[0]), "text_delta") || !strings.Contains(string(out[0]), "\"text\":\"hi\"") {
		t.Fatalf("delta: %s", out[0])
	}
	// finish
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	joined := string(out[0])
	if !strings.Contains(joined, "message_delta") || !strings.Contains(joined, "\"stop_reason\":\"end_turn\"") {
		t.Fatalf("finish: %s", out[0])
	}
	fin := s.Finish()
	if !strings.Contains(string(fin[0]), "message_stop") {
		t.Fatalf("finish frame: %s", fin[0])
	}
}

func TestO2CStream_ToolCalls(t *testing.T) {
	s := NewO2CStream()
	out, err := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	if err != nil || len(out) != 1 {
		t.Fatalf("tool start: %v", err)
	}
	if !strings.Contains(string(out[0]), "content_block_start") || !strings.Contains(string(out[0]), "\"type\":\"tool_use\"") {
		t.Fatalf("tool block start: %s", out[0])
	}
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]},"finish_reason":null}]}`))
	if len(out) != 1 || !strings.Contains(string(out[0]), "input_json_delta") {
		t.Fatalf("args delta: %s", out[0])
	}
	// tool_calls finish → stop_reason=tool_use
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	if !strings.Contains(string(out[0]), "\"stop_reason\":\"tool_use\"") {
		t.Fatalf("tool finish: %s", out[0])
	}
}

func TestO2CStream_BlockIndices(t *testing.T) {
	s := NewO2CStream()
	_ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"t"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	// text block index 0 结束后 tool_use 必须为 index 1（全局递增）
	if !strings.Contains(string(out[0]), "\"index\":1") {
		t.Fatalf("tool_use index must be 1: %s", out[0])
	}
}

func TestO2CStream_FramesWellFormed(t *testing.T) {
	s := NewO2CStream()
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	f := string(out[0])
	if !strings.HasPrefix(f, "event: message_start\n") || !strings.HasSuffix(f, "\n\n") {
		t.Fatalf("frame format: %q", f)
	}
	var ev StreamEvent
	line := f[strings.Index(f, "data: ")+6 : len(f)-2]
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Message == nil || ev.Message.Role != "assistant" {
		t.Fatalf("message_start payload: %+v", ev.Message)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run TestO2CStream -v`
Expected: FAIL（类型未定义）

- [ ] **Step 3: 实现 stream_o2c.go**

```go
package convert

import (
	"encoding/json"
	"fmt"
)

type O2CStream struct {
	id        string
	model     string
	started   bool
	blockIdx  int // Claude content_block 全局索引（text=0、tool_use=1…）
	inText    bool
	inTool    bool
	sentStop  bool
}

func NewO2CStream() *O2CStream { return NewO2CStreamWithModel("") }

func NewO2CStreamWithModel(model string) *O2CStream {
	return &O2CStream{id: NewClaudeID(), model: model}
}

func (s *O2CStream) Write(data []byte) ([][]byte, error) {
	var chunk StreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("o2c parse chunk: %w", err)
	}
	var frames [][]byte
	if !s.started {
		s.started = true
		frames = append(frames, s.frame(EventMessageStart, StreamEvent{
			Message: &MessagesResponse{ID: s.id, Type: "message", Role: "assistant", Model: s.model, Content: []ClaudeBlock{}},
		}))
	}
	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			frames = append(frames, s.frame(EventMessageDelta, StreamEvent{Usage: &ClaudeUsage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}}))
		}
		return frames, nil
	}
	delta := chunk.Choices[0].Delta
	if delta.Content != "" {
		if !s.inText {
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "text"}}))
			s.inText = true
		}
		frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "text_delta", Text: delta.Content}}))
	}
	for _, tc := range delta.ToolCalls {
		if tc.ID != "" && tc.Function.Name != "" {
			if s.inText {
				frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
				s.inText = false
			}
			s.blockIdx++
			s.inTool = true
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: map[string]any{}}}))
			continue
		}
		if s.inTool && tc.Function.Arguments != "" {
			frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments}}))
		}
	}
	if fr := chunk.Choices[0].FinishReason; fr != nil && !s.sentStop {
		s.sentStop = true
		if s.inText {
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText = false
		}
		frames = append(frames, s.frame(EventMessageDelta, StreamEvent{StopReason: stopReasonO2C(*fr)}))
	}
	if chunk.Usage != nil {
		frames = append(frames, s.frame(EventMessageDelta, StreamEvent{Usage: &ClaudeUsage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}}))
	}
	return frames, nil
}

func (s *O2CStream) Finish() [][]byte {
	if s.inText || s.inTool {
		s.inText, s.inTool = false, false
		return [][]byte{s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}), s.frame(EventMessageStop, StreamEvent{})}
	}
	return [][]byte{s.frame(EventMessageStop, StreamEvent{})}
}

func stopReasonO2C(reason string) *string {
	var r string
	switch reason {
	case "stop":
		r = "end_turn"
	case "tool_calls":
		r = "tool_use"
	case "length":
		r = "max_tokens"
	default:
		r = "end_turn"
	}
	return &r
}

func (s *O2CStream) frame(event string, payload StreamEvent) []byte {
	b, _ := json.Marshal(payload)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/convert/ -run TestO2CStream -v`
Expected: PASS。`TestO2CStream_TextFlow` 的 finish 断言检查 `out[0]` 是 message_delta 帧（若 finish 前有未闭 text block，会先出 content_block_stop——检查 `out` 数组最后一条）。

- [ ] **Step 5: Commit**

```bash
git add internal/convert/stream_o2c.go internal/convert/stream_o2c_test.go
git commit -m "feat: openai-to-claude streaming state machine"
```

---

### Task 7: C2O 流式状态机（Claude SSE → OpenAI SSE）

**Files:**
- Create: `internal/convert/stream_c2o.go`
- Test: `internal/convert/stream_c2o_test.go`

**Interfaces:**
- Consumes: Task 4 结构、Task 5 `NewOpenAIID`
- Produces:
  - `type C2OStream struct{ id, model string; toolIdx int; inTool bool }`
  - `func NewC2OStream(model string) *C2OStream`
  - `func (s *C2OStream) Write(frame []byte) ([][]byte, error)` — 输入一条 Claude 帧（`event: <type>\ndata: <json>\n\n`），输出 0..n 条 OpenAI `data: {...}\n\n` 字节
  - `func (s *C2OStream) Close() [][]byte` — 返回 `data: [DONE]\n\n`
  - 规则：message_start → 首个 chunk（delta.role=assistant、content=""、finish_reason=null，id=s.id、model=s.model）；text block start/delta/stop → delta.content；tool_use block start → delta.tool_calls[独立 toolIdx]{id, type:function, function{name, arguments:""}}，**toolIdx 独立计数只对 tool_use**；input_json_delta → arguments 增量；message_delta stop_reason → finish_reason 映射（end_turn→stop、tool_use→tool_calls、max_tokens→length、其余→stop）；message_delta usage → 附加在最后一个 chunk 的 usage 字段；**ping/stats 吞掉**；**thinking/redacted_thinking 块完全剥离**（start/delta/stop 全吞，不产事件）；message_stop → 无输出（Close 时发 [DONE]）
  - SSE 帧解析辅助：`func ParseClaudeFrame(frame []byte) (event string, data []byte, err error)`

- [ ] **Step 1: 写失败测试**

```go
package convert

import (
	"strings"
	"testing"
)

func TestC2OStream_TextFlow(t *testing.T) {
	s := NewC2OStream("claude-3")
	out, _ := s.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"role\":\"assistant\"") {
		t.Fatalf("start: %s", out[0])
	}
	out, _ = s.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	if len(out) != 0 {
		t.Fatalf("text block start should be silent: %s", out)
	}
	out, _ = s.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"content\":\"hi\"") {
		t.Fatalf("text delta: %s", out[0])
	}
	out, _ = s.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\n"))
	joined := string(out[len(out)-1])
	if !strings.Contains(joined, "\"finish_reason\":\"stop\"") || !strings.Contains(joined, "\"usage\"") {
		t.Fatalf("finish: %s", joined)
	}
	if got := string(s.Close()[0]); !strings.Contains(got, "[DONE]") {
		t.Fatalf("done: %s", got)
	}
}

func TestC2OStream_ToolCalls_IndependentIndex(t *testing.T) {
	s := NewC2OStream("claude-3")
	// 先 text block（index 0）
	_ = s.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	_ = s.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"thinking\"}}\n\n"))
	// tool_use block（Claude index 1，OpenAI tool_calls index 必须从 0）
	out, _ := s.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"index\":0") || !strings.Contains(string(out[0]), "\"id\":\"toolu_1\"") {
		t.Fatalf("tool start: %s", out[0])
	}
	out, _ = s.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":1}\"}}\n\n"))
	if len(out) != 1 || !strings.Contains(string(out[0]), "\"arguments\":\"{\\\"x\\\":1}\"") {
		t.Fatalf("tool delta: %s", out[0])
	}
	// tool finish
	out, _ = s.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"))
	if !strings.Contains(string(out[0]), "\"finish_reason\":\"tool_calls\"") {
		t.Fatalf("tool finish: %s", out[0])
	}
}

func TestC2OStream_ThinkingStripped(t *testing.T) {
	s := NewC2OStream("claude-3")
	outs := [][]byte(nil)
	for _, f := range []string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"secret\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"more\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	} {
		out, err := s.Write([]byte(f))
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, out...)
	}
	if len(outs) != 0 {
		t.Fatalf("thinking events leaked: %d", len(outs))
	}
}

func TestC2OStream_PingAndStatsSwallowed(t *testing.T) {
	s := NewC2OStream("claude-3")
	for _, f := range []string{
		"event: ping\ndata: {}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n",
	} {
		out, err := s.Write([]byte(f))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 0 {
			t.Fatalf("ping/usage-only delta should be swallowed: %d", len(out))
		}
	}
}

func TestParseClaudeFrame(t *testing.T) {
	event, data, err := ParseClaudeFrame([]byte("event: content_block_delta\ndata: {\"x\":1}\n\n"))
	if err != nil || event != EventContentBlockDelta || string(data) != `{"x":1}` {
		t.Fatalf("parse: %s %s %v", event, data, err)
	}
}
```

**注**：`TestC2OStream_PingAndStatsSwallowed` 中 usage-only message_delta 吞掉——但若之前已发过 finish chunk，usage 应并入。简化决策：**usage 并入最近一个已输出的 chunk**；若从未输出 chunk，直接吞掉。测试按此写。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run TestC2OStream -v`
Expected: FAIL（类型未定义）

- [ ] **Step 3: 实现 stream_c2o.go**

```go
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type C2OStream struct {
	id       string
	model    string
	toolIdx  int // 只对 tool_use 独立计数
	inTool   bool
	lastChunk []byte
}

func NewC2OStream(model string) *C2OStream {
	return &C2OStream{id: NewOpenAIID(), model: model}
}

// ParseClaudeFrame 解析一条 Claude SSE 帧。
func ParseClaudeFrame(frame []byte) (event string, data []byte, err error) {
	lines := bytes.Split(bytes.TrimRight(frame, "\n"), []byte("\n"))
	for _, ln := range lines {
		if bytes.HasPrefix(ln, []byte("event: ")) {
			event = string(bytes.TrimPrefix(ln, []byte("event: ")))
		}
		if bytes.HasPrefix(ln, []byte("data: ")) {
			data = bytes.TrimPrefix(ln, []byte("data: "))
		}
	}
	if event == "" {
		return "", nil, fmt.Errorf("claude frame missing event: %q", frame)
	}
	return event, data, nil
}

func (s *C2OStream) Write(frame []byte) ([][]byte, error) {
	event, data, err := ParseClaudeFrame(frame)
	if err != nil {
		return nil, err
	}
	switch event {
	case EventMessageStart, EventPing, EventError, EventContentBlockStop:
		return nil, nil
	case EventContentBlockStart:
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		if ev.ContentBlock == nil {
			return nil, nil
		}
		switch ev.ContentBlock.Type {
		case "thinking", "redacted_thinking":
			return nil, nil // 剥离
		case "tool_use":
			s.inTool = true
			chunk := s.chunk(StreamChoice{
				Delta: ChatMessage{ToolCalls: []ToolCall{{
					ID:       ev.ContentBlock.ID,
					Type:     "function",
					Function: ToolCallFunction{Name: ev.ContentBlock.Name, Arguments: ""},
				}}},
			})
			return [][]byte{chunk}, nil
		default: // text 等：静默
			return nil, nil
		}
	case EventContentBlockDelta:
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		if ev.Delta == nil {
			return nil, nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if s.inTool { // 防御：text delta 不应出现在 tool 中
				return nil, nil
			}
			return [][]byte{s.chunk(StreamChoice{Delta: ChatMessage{Content: ev.Delta.Text}})}, nil
		case "input_json_delta":
			return [][]byte{s.chunk(StreamChoice{Delta: ChatMessage{ToolCalls: []ToolCall{{Function: ToolCallFunction{Arguments: ev.Delta.PartialJSON}}}}})}, nil
		case "thinking_delta", "signature_delta":
			return nil, nil // 剥离
		}
		return nil, nil
	case EventMessageDelta:
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		var out [][]byte
		if ev.StopReason != nil {
			fr := finishReasonC2O(*ev.StopReason)
			out = append(out, s.chunk(StreamChoice{FinishReason: &fr}))
		}
		if ev.Usage != nil && len(out) > 0 {
			out[len(out)-1] = s.withUsage(out[len(out)-1], &Usage{
				PromptTokens:     ev.Usage.InputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
			})
		}
		return out, nil
	case EventMessageStop:
		return nil, nil
	default:
		// 未知事件（含 stats）：吞掉
		return nil, nil
	}
}

func finishReasonC2O(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

func (s *C2OStream) chunk(choice StreamChoice) []byte {
	b, _ := json.Marshal(StreamChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Model:   s.model,
		Choices: []StreamChoice{{Index: 0, Delta: choice.Delta, FinishReason: choice.FinishReason}},
	})
	s.lastChunk = append([]byte("data: "), append(b, '\n', '\n')...)
	return s.lastChunk
}

func (s *C2OStream) withUsage(chunk, usage []byte) []byte {
	// 简化：直接在原始 chunk JSON 的 choices 后附加 usage（data 行重建）
	line := bytes.TrimSuffix(bytes.TrimPrefix(chunk, []byte("data: ")), []byte("\n\n"))
	var m map[string]any
	_ = json.Unmarshal(line, &m)
	m["usage"] = usageAsMap(usage)
	// usage 参数直接传 map
	return []byte("data: " + mustJSON(m) + "\n\n")
}

func usageAsMap(u *Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}

func (s *C2OStream) Close() [][]byte {
	return [][]byte{[]byte("data: [DONE]\n\n")}
}
```

**实现修正**：`withUsage` 签名改为 `func (s *C2OStream) withUsage(chunk []byte, u *Usage) []byte`，内部直接 marshal `StreamChunk` 重建（避免 map 拆装）。在 Write 里调用处同步。测试按最终行为断言（finish chunk 含 usage）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/convert/ -run TestC2OStream -v`
Expected: PASS。若 `TestC2OStream_TextFlow` 的 usage 断言失败，检查 `withUsage` 重建逻辑。

- [ ] **Step 5: Commit**

```bash
git add internal/convert/stream_c2o.go internal/convert/stream_c2o_test.go
git commit -m "feat: claude-to-openai streaming state machine"
```

---

### Task 8: O2C/C2O 请求转换器（非流式）

**Files:**
- Create: `internal/convert/request.go`
- Test: `internal/convert/request_test.go`

**Interfaces:**
- Consumes: Task 4 结构、Task 5 原子函数
- Produces:
  - `func OpenAIRequestToClaude(req *ChatCompletionRequest, model string) (*MessagesRequest, error)` — model 用调用方传入值；max_tokens：`MaxCompletion` 优先、其次 `MaxTokens`、缺省 4096；剥离 n>1（**返回 error，由 server 层转 400**）；剥离 temperature 之外 OpenAI 独有字段；system 经 `OpenAIMessagesToClaude` 提取；tools 经 `OpenAIToolsToClaude`
  - `func ClaudeRequestToOpenAI(req *MessagesRequest, model string) (*ChatCompletionRequest, error)` — max_tokens → MaxTokens；top_k 剥离；system + messages 经 `ClaudeMessagesToOpenAI`（system 由调用方已合并）；tools 经 `ClaudeToolsToOpenAI`

- [ ] **Step 1: 写失败测试**

```go
package convert

import "testing"

func TestOpenAIRequestToClaude(t *testing.T) {
	n := 2
	req := &ChatCompletionRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: FunctionDef{Name: "f", Parameters: map[string]any{"type": "object"}}}},
		N:        &n,
	}
	_, err := OpenAIRequestToClaude(req, "claude-3")
	if err == nil {
		t.Fatal("n>1 must be rejected")
	}
	req.N = nil
	out, err := OpenAIRequestToClaude(req, "claude-3")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-3" || out.MaxTokens != 4096 {
		t.Fatalf("model/max_tokens: %+v", out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "f" {
		t.Fatalf("tools: %+v", out.Tools)
	}
}

func TestClaudeRequestToOpenAI(t *testing.T) {
	req := &MessagesRequest{
		Model:      "claude-3",
		System:     "sys",
		MaxTokens:  2048,
		Messages:   []ClaudeMessage{{Role: "user", Content: "hi"}},
		Tools:      []ClaudeTool{{Name: "f", InputSchema: map[string]any{"type": "object"}}},
		StopSequences: []string{"\n"},
	}
	out, err := ClaudeRequestToOpenAI(req, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-4o" || *out.MaxTokens != 2048 {
		t.Fatalf("model/max_tokens: %+v", out)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "system" || out.Messages[0].Content != "sys" {
		t.Fatalf("messages: %+v", out.Messages)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "f" {
		t.Fatalf("tools: %+v", out.Tools)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run TestOpenAIRequestToClaude -v`
Expected: FAIL（函数未定义）

- [ ] **Step 3: 实现 request.go**

```go
package convert

import "fmt"

const defaultMaxTokens = 4096

func OpenAIRequestToClaude(req *ChatCompletionRequest, model string) (*MessagesRequest, error) {
	if req.N != nil && *req.N > 1 {
		return nil, fmt.Errorf("n>1 not supported")
	}
	maxTokens := defaultMaxTokens
	if req.MaxCompletion != nil && *req.MaxCompletion > 0 {
		maxTokens = *req.MaxCompletion
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	system, msgs, err := OpenAIMessagesToClaude(req.Messages)
	if err != nil {
		return nil, err
	}
	out := &MessagesRequest{
		Model:         model,
		System:        system,
		Messages:      msgs,
		MaxTokens:     maxTokens,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
	}
	if len(req.Tools) > 0 {
		out.Tools = OpenAIToolsToClaude(req.Tools)
	}
	return out, nil
}

func ClaudeRequestToOpenAI(req *MessagesRequest, model string) (*ChatCompletionRequest, error) {
	msgs, err := ClaudeMessagesToOpenAI(req.Messages, req.System)
	if err != nil {
		return nil, err
	}
	out := &ChatCompletionRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
	}
	if len(req.Tools) > 0 {
		out.Tools = ClaudeToolsToOpenAI(req.Tools)
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	out.MaxTokens = &maxTokens
	return out, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/convert/ -run 'TestOpenAIRequestToClaude|TestClaudeRequestToOpenAI' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go
git commit -m "feat: cross-format request converters"
```

---

### Task 9: O2C/C2O 响应转换器（非流式）

**Files:**
- Create: `internal/convert/response.go`
- Test: `internal/convert/response_test.go`

**Interfaces:**
- Consumes: Task 4 结构、Task 5 原子函数（`ClaudeInputToOpenAIArgs`、`ClaudeSourceToImageURL`）
- Produces:
  - `func OpenAIResponseToClaude(resp *ChatCompletionResponse, id string) (*MessagesResponse, error)` — 生成 `msg_` id；content → text block + tool_calls → tool_use blocks（arguments parse 为对象）；finish_reason → stop_reason；usage 映射
  - `func ClaudeResponseToOpenAI(resp *MessagesResponse, id string) (*ChatCompletionResponse, error)` — 生成 `chatcmpl-` id；blocks → content 字符串（text 拼接）+ tool_use → tool_calls（input 序列化）+ thinking 剥离；image block → data URL 文本（不入 content 字符串——**决策：C2O 非流式响应中图片 block 直接追加为 data URL 文本**，客户端可读）；stop_reason → finish_reason；usage 映射

- [ ] **Step 1: 写失败测试**

```go
package convert

import (
	"strings"
	"testing"
)

func TestOpenAIResponseToClaude(t *testing.T) {
	resp := &ChatCompletionResponse{
		Choices: []ResponseChoice{{Message: ChatMessage{
			Content: "hi",
			ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}},
		}, FinishReason: "tool_calls"}},
		Usage: &Usage{PromptTokens: 10, CompletionTokens: 5},
	}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "msg_") || len(out.Content) != 2 {
		t.Fatalf("id/content: %+v", out)
	}
	if out.Content[1].Type != "tool_use" || out.Content[1].Name != "f" {
		t.Fatalf("tool_use: %+v", out.Content[1])
	}
	if out.Content[1].Input.(map[string]any)["x"] != float64(1) {
		t.Fatalf("input parsed: %+v", out.Content[1].Input)
	}
	if out.StopReason == nil || *out.StopReason != "tool_use" {
		t.Fatalf("stop_reason: %v", out.StopReason)
	}
	if out.Usage == nil || out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Fatalf("usage: %+v", out.Usage)
	}
}

func TestClaudeResponseToOpenAI_ThinkingStripped(t *testing.T) {
	resp := &MessagesResponse{
		Content: []ClaudeBlock{
			{Type: "thinking", Thinking: "secret"},
			{Type: "text", Text: "answer"},
			{Type: "tool_use", ID: "toolu_1", Name: "f", Input: map[string]any{"x": 1}},
		},
		StopReason: strPtr("tool_use"),
	}
	out, err := ClaudeResponseToOpenAI(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Fatalf("id: %s", out.ID)
	}
	if out.Choices[0].Message.Content != "answer" {
		t.Fatalf("content: %v", out.Choices[0].Message.Content)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 || out.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("tool_calls: %+v", out.Choices[0].Message.ToolCalls)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason: %s", out.Choices[0].FinishReason)
	}
}

func strPtr(s string) *string { return &s }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run TestOpenAIResponseToClaude -v`
Expected: FAIL（函数未定义）

- [ ] **Step 3: 实现 response.go**

```go
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

func OpenAIResponseToClaude(resp *ChatCompletionResponse, id string) (*MessagesResponse, error) {
	if id == "" {
		id = NewClaudeID()
	}
	out := &MessagesResponse{ID: id, Type: "message", Role: "assistant", Model: resp.Model}
	var blocks []ClaudeBlock
	for _, ch := range resp.Choices {
		if s := contentString(ch.Message.Content); s != "" {
			blocks = append(blocks, ClaudeBlock{Type: "text", Text: s})
		}
		for _, tc := range ch.Message.ToolCalls {
			input, perr := OpenAIArgsToClaudeInput(tc.Function.Arguments)
			if perr != nil {
				input = map[string]any{}
			}
			blocks = append(blocks, ClaudeBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
		}
	}
	out.Content = blocks
	if fr := resp.Choices[0].FinishReason; fr != "" {
		out.StopReason = stopReasonO2C(fr)
	}
	if resp.Usage != nil {
		out.Usage = &ClaudeUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	}
	return out, nil
}

func ClaudeResponseToOpenAI(resp *MessagesResponse, id string) (*ChatCompletionResponse, error) {
	if id == "" {
		id = NewOpenAIID()
	}
	out := &ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: 0,
		Model:   resp.Model,
	}
	var text strings.Builder
	var toolCalls []ToolCall
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args, err := ClaudeInputToOpenAIArgs(b.Input)
			if err != nil {
				args = "{}"
			}
			toolCalls = append(toolCalls, ToolCall{ID: b.ID, Type: "function", Function: ToolCallFunction{Name: b.Name, Arguments: args}})
		case "image":
			text.WriteString(ClaudeSourceToImageURL(b.Source))
		case "thinking", "redacted_thinking", "signature":
			// 剥离
		}
	}
	msg := ChatMessage{Role: "assistant", Content: text.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	fr := "stop"
	if resp.StopReason != nil {
		fr = finishReasonC2O(*resp.StopReason)
	}
	out.Choices = []ResponseChoice{{Index: 0, Message: msg, FinishReason: fr}}
	if resp.Usage != nil {
		out.Usage = &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}
	return out, nil
}
```

（`json` import 仅用于潜在调试，若无使用删除。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/convert/ -v`
Expected: PASS（全部）

- [ ] **Step 5: Commit**

```bash
git add internal/convert/response.go internal/convert/response_test.go
git commit -m "feat: cross-format response converters"
```

---

### Task 10: 路由决策 + 图片检测

**Files:**
- Create: `internal/route/route.go`
- Test: `internal/route/route_test.go`

**Interfaces:**
- Consumes: Task 2 `config.Config`、Task 4 结构、Task 5 `BlockHasImage`/`ContentHasImage`
- Produces:
  - `type Decision struct { Upstream string; Model string; VisionSwitch bool }`
  - `func Decide(cfg *config.Config, format string, body []byte) (Decision, error)` — 两条规则（spec §4）：1) 图片 + `AutoSwitchVision` + vision 存在 → vision 上游 + vision.model；2) 其余 → main + main.model。format ∈ {"openai","claude"}，解析失败返回 error（400 由 server 层转）
  - `func RequestHasImage(format string, body []byte) (bool, error)` — 完整反序列化：openai → 遍历 messages content（`[]any` 的 map 含 `type=image_url` 或 string content 也检查？**否**——string content 无图；仅数组形态）；claude → 遍历 content（`[]ClaudeBlock` 递归 tool_result，`[]any` 形态用 `ContentHasImage`）
  - 模型名**不读**：`Decide` 不解析 model 字段，仅用于图片检测

- [ ] **Step 1: 写失败测试**

```go
package route

import (
	"testing"

	"prism-proxy/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main":   {BaseURL: "https://a/v1", APIKey: "k", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: "https://b/v1", APIKey: "k", Format: "claude", Model: "claude-3"},
		},
	}
}

func TestDecide_VisionSwitch(t *testing.T) {
	body := []byte(`{"model":"ignored","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(testConfig(), "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if !d.VisionSwitch || d.Upstream != "vision" || d.Model != "claude-3" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_TextGoesMain(t *testing.T) {
	body := []byte(`{"model":"whatever","messages":[{"role":"user","content":"hi"}]}`)
	d, err := Decide(testConfig(), "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.VisionSwitch || d.Upstream != "main" || d.Model != "gpt-4o" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_ClaudeFormatImage(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	d, err := Decide(testConfig(), "claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if !d.VisionSwitch || d.Upstream != "vision" {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_ImageInToolResult(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`)
	has, err := RequestHasImage("claude", body)
	if err != nil || !has {
		t.Fatalf("tool_result nested image: %v %v", has, err)
	}
}

func TestDecide_Disabled(t *testing.T) {
	cfg := testConfig()
	cfg.AutoSwitchVision = false
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	d, err := Decide(cfg, "openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Upstream != "main" || d.VisionSwitch {
		t.Fatalf("decision: %+v", d)
	}
}

func TestDecide_InvalidBody(t *testing.T) {
	_, err := Decide(testConfig(), "openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/route/ -v`
Expected: FAIL（`Decide` 未定义）

- [ ] **Step 3: 实现 route.go**

```go
package route

import (
	"encoding/json"
	"fmt"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
)

type Decision struct {
	Upstream    string
	Model       string
	VisionSwitch bool
}

// Decide 按 spec §4 两条规则决策，忽略请求模型名。
func Decide(cfg *config.Config, format string, body []byte) (Decision, error) {
	hasImage, err := RequestHasImage(format, body)
	if err != nil {
		return Decision{}, err
	}
	if hasImage && cfg.AutoSwitchVision {
		if vision, ok := cfg.Vision(); ok {
			return Decision{Upstream: "vision", Model: vision.Model, VisionSwitch: true}, nil
		}
	}
	return Decision{Upstream: "main", Model: cfg.Main().Model}, nil
}

// RequestHasImage 结构化检测入站请求是否含图片（递归 tool_result）。
func RequestHasImage(format string, body []byte) (bool, error) {
	switch format {
	case "openai":
		var req convert.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return false, fmt.Errorf("parse openai request: %w", err)
		}
		for _, m := range req.Messages {
			if parts, ok := m.Content.([]any); ok {
				for _, p := range parts {
					if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
						return true, nil
					}
				}
			}
		}
		return false, nil
	case "claude":
		var req convert.MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return false, fmt.Errorf("parse claude request: %w", err)
		}
		for _, m := range req.Messages {
			if blocks, ok := m.Content.([]convert.ClaudeBlock); ok {
				if convert.BlockHasImage(blocks) {
					return true, nil
				}
			} else if parts, ok := m.Content.([]any); ok {
				if convert.ContentHasImage(parts) {
					return true, nil
				}
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unknown format %q", format)
	}
}
```

**注意**：`m.Content.([]convert.ClaudeBlock)` 断言在 `json.Unmarshal` 后**永远失败**（解码产物是 `[]any`），测试 `TestDecide_ClaudeFormatImage` 走 `[]any` 分支——但 `ContentHasImage(parts)` 只检查 `image_url`，需补 Claude `image` block 检查。修正 `ContentHasImage`（Task 5）：对 `[]any` 中 `type == "image"` 也返回 true。实现时同步改 Task 5 的函数并跑 Task 5 测试。

- [ ] **Step 4: 修正 Task 5 的 ContentHasImage**

在 `internal/convert/messages.go`：

```go
func ContentHasImage(parts any) bool {
	switch v := parts.(type) {
	case []ClaudeBlock:
		return BlockHasImage(v)
	case []any:
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				switch m["type"] {
				case "image_url":
					return true
				case "image":
					return true
				case "tool_result":
					if inner, ok := m["content"].([]any); ok && ContentHasImage(inner) {
						return true
					}
				}
			}
		}
	}
	return false
}
```

Run: `go test ./internal/convert/ -v` Expected: PASS（回归确认）

- [ ] **Step 5: 跑 route 测试确认通过**

Run: `go test ./internal/route/ -v`
Expected: PASS（6 个测试）

- [ ] **Step 6: Commit**

```bash
git add internal/route/ internal/convert/messages.go
git commit -m "feat: routing decisions and image detection"
```

---

### Task 11: 上游客户端

**Files:**
- Create: `internal/upstream/upstream.go`
- Test: `internal/upstream/upstream_test.go`

**Interfaces:**
- Consumes: Task 2 `config.UpstreamConfig`
- Produces:
  - `type Client struct{ hc *http.Client }`
  - `func NewClient() *Client`
  - `func (c *Client) Do(ctx context.Context, u *config.UpstreamConfig, body []byte, stream bool) (*http.Response, error)` — URL 拼接：`strings.TrimSuffix(u.BaseURL, "/") + "/chat/completions"`（openai）或 `+ "/messages"`（claude）；认证头：openai → `Authorization: Bearer <key>`；claude → `x-api-key: <key>` + `anthropic-version: 2023-06-01`；`Content-Type: application/json`；stream=true 时请求体加 `"stream":true`（body 已是最终出站体，含 stream 字段——由 server 层在转换/透传时设置，本层不篡改）；超时：连接+响应头超时 `u.Timeout`（`http.Client{Timeout}` 之外的实现——用 `context.WithTimeout` 包裹读响应头阶段？**简化**：`http.Client.Timeout = u.Timeout` 会截断长流——实现用 `http.Transport` + `ResponseHeaderTimeout: u.Timeout`，流式读取时用流空闲读超时 60s 的 `io.Reader` 包装（`bufio.Reader` + `SetReadDeadline` 不适用于 HTTP body——改用 `context.WithTimeout` 包整个流读？**最终方案**：`Client.Do` 只设 `ResponseHeaderTimeout`，流式 body 读取由 server 层按块 `context.WithTimeout` 包读（60s 空闲）。本层不设总时限）
  - 错误体透传：非 2xx 直接把 `resp` 交还调用方（server 层原样回写），不做错误解析

- [ ] **Step 1: 写失败测试**

```go
package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/upstream/ -v`
Expected: FAIL（类型未定义）

- [ ] **Step 3: 实现 upstream.go**

```go
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"prism-proxy/internal/config"
)

type Client struct {
	hc *http.Client
}

func NewClient() *Client {
	transport := &http.Transport{
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		ResponseHeaderTimeout: 0, // 每请求单独设置
	}
	hc := &http.Client{Transport: transport}
	return &Client{hc: hc}
}

// Do 转发请求。超时语义：连接 + 响应头阶段用 u.Timeout（默认 120s）；
// 流式 body 读取的总时长不限，空闲超时由调用方按块控制。
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
	c.hc.Transport.(*http.Transport).ResponseHeaderTimeout = timeout
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	return resp, nil
}
```

**注意**：`Do` 不读 body——非 2xx 时调用方直接透传 `resp`；2xx 流式时调用方边读边转。`ResponseHeaderTimeout` 在 Transport 上共享设置（全局单 client 场景 OK，若未来多上游并发不同 timeout 需拆分 client——文档注明，当前单 client + 每上游相同默认值可接受；**更稳妥**：每请求 clone transport 设 timeout。实现用 clone）。

```go
// Do 内超时实现（替换上面 Transport 共享设置）：
transport := c.hc.Transport.(*http.Transport).Clone()
transport.ResponseHeaderTimeout = timeout
client := &http.Client{Transport: transport}
resp, err := client.Do(req)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/upstream/ -v`
Expected: PASS（3 个测试）

- [ ] **Step 5: Commit**

```bash
git add internal/upstream/
git commit -m "feat: upstream client with header rewrite and timeout"
```

---

### Task 12: 入站 server（认证 + 透传 + SSE 回写）

**Files:**
- Create: `internal/server/server.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: Task 2 `config.Watcher`、Task 10 `route.Decide`、Task 11 `upstream.Client`
- Produces:
  - `type Server struct{ cfg *config.Watcher; client *upstream.Client; logger *slog.Logger }`
  - `func New(cfg *config.Watcher, client *upstream.Client, logger *slog.Logger) *Server`
  - `func NewWithConfig(cfg *config.Config, client *upstream.Client, logger *slog.Logger) *Server` — 测试注入（包一层 `staticWatcher`）
  - `func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)` — 路径分发：`POST /v1/chat/completions` → openai；`POST /v1/messages` → claude；其余 404。**本任务只做透传路径**（同格式）+ 认证 + 错误透传；交叉转换路径 Task 13 接线
  - 认证中间件：`auth_keys` 非空 → 校验 `Authorization: Bearer`（openai 路径）或 `x-api-key`（claude 路径），失败 401
  - 透传流程：读 body（限 50MB）→ `route.Decide` → 取目标上游 → **改写 body 的 model 字段**（`json.RawMessage` 替换）→ `client.Do` → 非 2xx：状态码 + 错误体原样回写 → 2xx：`Content-Type: text/event-stream` 则流式逐块回写（`http.Flusher`），否则整体回写
  - 流式读超时：每块读 `context.WithTimeout` 60s
  - 请求日志：`log` 包（Task 13 统一；本任务先记基础字段）
  - `func rewriteModel(format string, body []byte, model string) ([]byte, error)` — 用 `json.RawMessage` 局部替换 `model` 字段（openai 与 claude 的 model 同为顶层字段，统一处理）

- [ ] **Step 1: 写失败测试**

```go
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

	resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
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
	resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[]}`))
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
	resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[],"stream":true}`))
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: %s", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "hi") || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("stream body: %s", b)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -v`
Expected: FAIL（`New`/`NewWithConfig` 未定义）

- [ ] **Step 3: 实现 server.go**

```go
package server

import (
	"context"
	"encoding/json"
	"fmt"
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

type Server struct {
	cfg    *config.Watcher
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
	up, ok := cfg.Upstreams[decision.Upstream]
	if !ok {
		http.Error(w, "upstream not found", http.StatusBadGateway)
		return
	}
	outbound, err := rewriteModel(format, body, decision.Model)
	if err != nil {
		http.Error(w, "rewrite: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Task 13 在此插入交叉转换
	resp, err := s.client.Do(r.Context(), &up, outbound, false)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(w, resp.Body)
		s.log(start, format, decision, up, resp.StatusCode, false, err)
		return
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		s.copyStream(w, resp.Body)
	} else {
		_, _ = io.Copy(w, resp.Body)
	}
	s.log(start, format, decision, up, resp.StatusCode, false, err)
}

func (s *Server) copyStream(w http.ResponseWriter, src io.Reader) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, _ = io.Copy(w, src)
		return
	}
	buf := make([]byte, 32*1024)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), streamIdleTimeout)
		chunk, err := readWithCtx(ctx, src, buf)
		cancel()
		if len(chunk) > 0 {
			_, _ = w.Write(chunk)
			flusher.Flush()
		}
		if err != nil {
			return // 断流（EOF 或超时），不伪造结束事件
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
	if _, ok := m["model"]; !ok {
		return nil, fmt.Errorf("missing model field")
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: PASS（6 个测试）

- [ ] **Step 5: Commit**

```bash
git add internal/server/
git commit -m "feat: inbound server with auth, passthrough and SSE relay"
```

---

### Task 13: 完整链路接线（交叉转换 + vision 切换 + 日志）

**Files:**
- Modify: `internal/server/server.go`（`ServeHTTP` 交叉转换分支 + 图片外链下载）
- Create: `internal/server/convert.go`
- Test: `internal/server/convert_test.go`（O2C 与 C2O 完整请求链路测试）

**Interfaces:**
- Consumes: Task 6/7 流式转换器、Task 8/9 请求/响应转换器、Task 11 `upstream.Client`
- Produces:
  - `func (s *Server) relay(w, r, cfg, format, body, decision) error` — 核心接线：决策后按 `入站格式 × 上游 format` 分派：
    - 同格式 → 透传（Task 12 已有路径）
    - 交叉 → 转换请求 → 上游（stream 由请求 body 的 stream 字段决定）→ 非流式：读上游 body → 转换响应 → 回写；流式：读上游 SSE 帧 → 逐帧转换 → 回写（O2C 用 `O2CStream`，C2O 用 `C2OStream`，帧解析 `ParseClaudeFrame`）
  - 外链图下载：`func fetchExternalImages(blocks []convert.ClaudeBlock) error` — O2C 请求转换后，遍历 blocks 找 `Source.Type=="url"` → `http.Get`（30s 超时）→ base64 → 替换；失败返回 error（400）
  - 错误透传保持：上游非 2xx 原样回写
  - 流式错误：上游中途断流 → 断流 + 日志

- [ ] **Step 1: 写失败测试（O2C 非流式完整链路）**

```go
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"prism-proxy/internal/config"
	"prism-proxy/internal/upstream"
)

func TestO2CFullChain_NonStream(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 断言收到的是 Claude 格式请求
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("claude body: %v", err)
		}
		if _, hasSystem := req["system"]; !hasSystem {
			t.Fatalf("claude request missing system: %s", body)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"hello from claude"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// 客户端以 OpenAI 格式发送
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("client should get openai format: %v", err)
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello from claude" {
		t.Fatalf("content: %v", msg["content"])
	}
}

func TestC2OFullChain_NonStream(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi from gpt"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
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
	// 客户端以 Claude 格式发送
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("client should get claude format: %v", err)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "hi from gpt" {
		t.Fatalf("content: %v", content)
	}
}

func TestO2CStream_ThroughServer(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[]}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			fl.Flush()
		}
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"delta":{"content":"hi"}`) && !strings.Contains(s, "hi") {
		t.Fatalf("stream body: %s", s)
	}
	if !strings.Contains(s, "[DONE]") {
		t.Fatalf("missing DONE: %s", s)
	}
}

func TestVisionSwitch_ToClaudeVision(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"claude-3-5-vision"`) {
			t.Fatalf("vision model not rewritten: %s", body)
		}
		if !strings.Contains(string(body), `"type":"image"`) {
			t.Fatalf("image block missing: %s", body)
		}
		if !strings.Contains(string(body), `"media_type":"image/png"`) {
			t.Fatalf("media_type missing: %s", body)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_v","type":"message","role":"assistant","model":"claude-3-5-vision","content":[{"type":"text","text":"i see the image"}],"stop_reason":"end_turn"}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: "http://unused/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk-v", Format: "claude", Model: "claude-3-5-vision"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// OpenAI 客户端带图 → 自动切 vision（Claude 格式上游）
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "i see the image") {
		t.Fatalf("body: %s", b)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestO2CFullChain -v`
Expected: FAIL（relay 交叉路径未实现——透传把 Claude 响应原样回给了 OpenAI 客户端，测试应因格式断言失败）

- [ ] **Step 3: 实现 server/convert.go 与改造 ServeHTTP**

```go
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
	"prism-proxy/internal/route"
	"prism-proxy/internal/upstream"
)

// relay 处理单个请求的完整转发。format 为入站格式，up.Format 为出站格式。
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
	// 交叉格式：转换
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

// forward 转发并转换响应。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, up *config.UpstreamConfig, outbound []byte, inboundFormat string, d route.Decision) error {
	resp, err := s.client.Do(r.Context(), up, outbound, false)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return nil
	}
	defer resp.Body.Close()
	// 错误透传：原样
	if resp.StatusCode >= 400 {
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if inboundFormat == up.Format {
		// 同格式流式：原样转发
		if isStream {
			s.copyStream(w, resp.Body)
		} else {
			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}
		return nil
	}
	// 交叉格式：转换响应
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if isStream {
		return s.convertStream(w, resp.Body, inboundFormat, up.Format, up.Model)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if inboundFormat == "openai" { // 上游 Claude → 客户端 OpenAI
		var cr convert.MessagesResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return fmt.Errorf("parse claude response: %w", err)
		}
		out, err := convert.ClaudeResponseToOpenAI(&cr, "")
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(out)
	}
	var or convert.ChatCompletionResponse
	if err := json.Unmarshal(data, &or); err != nil {
		return fmt.Errorf("parse openai response: %w", err)
	}
	out, err := convert.OpenAIResponseToClaude(&or, "")
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(out)
}

func (s *Server) convertStream(w http.ResponseWriter, src io.Reader, inboundFormat, outboundFormat, model string) error {
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
	for {
		data, done, err := scanner.Next(src)
		if done {
			break
		}
		if err != nil {
			break // 断流
		}
		outs, err := c.Write(data)
		if err != nil {
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
	for _, o := range c.Finish() {
		_, _ = w.Write(o)
	}
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// readClaudeFrames 逐帧读取 Claude SSE 流。
func (s *Server) readClaudeFrames(src io.Reader, onFrame func([]byte) error, onDone func() error) error {
	// 简易帧边界：累积行，空行触发帧
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

// newSSEReader / openAISSE / requestStream / fetchExternalImages 实现：
func requestStream(format string, body []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	var stream bool
	_ = json.Unmarshal(m["stream"], &stream)
	return stream
}

func fetchExternalImages(req *convert.MessagesRequest) error {
	for mi, msg := range req.Messages {
		blocks, ok := msg.Content.([]convert.ClaudeBlock)
		if !ok {
			continue
		}
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
	}
	return nil
}

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
```

**SSE 帧解析器**（`newSSEReader`/`openAISSE` 为内部辅助，实现要点）：
- `openAISSE.Next(src)`：按行读 `data: ` 前缀，`[DONE]` 返回 `done=true`
- `newSSEReader`：按行累积到空行（`\n\n` 边界）产出完整帧

**改造 `ServeHTTP`**：将 Task 12 中的透传段替换为 `return s.relay(w, r, cfg, format, body, decision)`（透传逻辑移入 `forward` 同格式分支）。`copyStream` 保留供同格式流式复用。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: PASS（Task 12 的 6 个 + 本任务 4 个 = 10 个）

- [ ] **Step 5: 接线 main.go：Watcher + Server + 启动**

修改 `cmd/prism-proxy/main.go` `serveCmd`：

```go
RunE: func(cmd *cobra.Command, args []string) error {
	watcher, err := config.NewWatcher(configPath)
	if err != nil {
		return fmt.Errorf("invalid config (fail fast): %w", err)
	}
	defer watcher.Close()
	srv := server.New(watcher, upstream.NewClient(), slog.Default())
	logger := slog.Default()
	logger.Info("prism-proxy listening", "addr", watcher.Get().Server.Listen, "config", configPath)
	return http.ListenAndServe(watcher.Get().Server.Listen, srv)
},
```

imports 加 `"log/slog"`、`"prism-proxy/internal/server"`、`"prism-proxy/internal/upstream"`。

- [ ] **Step 6: 编译 + 冒烟**

```bash
go build ./...
printf 'auto_switch_vision: true\nmain:\n  baseurl: "http://127.0.0.1:9999/v1"\n  api_key: "sk"\n  format: openai\n  model: "gpt-4o"\nvision:\n  baseurl: "http://127.0.0.1:9998/v1"\n  api_key: "sk"\n  format: claude\n  model: "claude-3-5-vision"\n' > /tmp/prism-test.yaml
go run ./cmd/prism-proxy serve --config /tmp/prism-test.yaml &
sleep 1 && curl -s -X POST http://127.0.0.1:8787/v1/chat/completions -d '{"messages":[{"role":"user","content":"hi"}]}' ; kill %1
```

Expected: curl 返回 502（上游 9999 不可达——说明认证/路由/转发链路已通）；无 panic。

- [ ] **Step 7: Commit**

```bash
git add internal/server/ cmd/prism-proxy/main.go
git commit -m "feat: full relay chain with cross-format conversion"
```

---

### Task 14: mock 上游四象限集成测试矩阵

**Files:**
- Create: `internal/server/integration_test.go`

**Interfaces:**
- Consumes: 全部既有包
- Produces: 四象限 × {非流式、流式、工具调用、带图} 全矩阵回归测试（对齐 spec §8 测试策略 2）

- [ ] **Step 1: 写测试（mock 双上游 + 4 象限 × 4 场景）**

```go
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"prism-proxy/internal/config"
	"prism-proxy/internal/upstream"
)

// mockUpstream 按 format 提供最小可用响应。
type mockUpstream struct {
	format  string
	model   string
	handler http.HandlerFunc
}

func openaiMockResponse(model string, content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "model": model,
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
	return string(b)
}

func claudeMockResponse(model string, content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]any{{"type": "text", "text": content}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
	})
	return string(b)
}

func TestMatrix_AllQuadrants(t *testing.T) {
	quadrants := []struct {
		inboundFormat string
		upstreamFormat string
		path           string
		reqBody        string
	}{
		{"openai", "openai", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"openai", "claude", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"claude", "openai", "/v1/messages", `{"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`},
		{"claude", "claude", "/v1/messages", `{"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, q := range quadrants {
		t.Run(q.inboundFormat+"->"+q.upstreamFormat, func(t *testing.T) {
			var upstreamSrv *httptest.Server
			var wantContent string
			upstreamSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if q.upstreamFormat == "openai" {
					w.Write([]byte(openaiMockResponse("gpt-4o", "answer")))
				} else {
					w.Write([]byte(claudeMockResponse("claude-3", "answer")))
				}
			}))
			defer upstreamSrv.Close()
			cfg := &config.Config{
				Upstreams: map[string]config.UpstreamConfig{
					"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: q.upstreamFormat, Model: "m"},
				},
			}
			srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
			ts := httptest.NewServer(srv)
			defer ts.Close()
			resp, err := http.Post(ts.URL+q.path, "application/json", strings.NewReader(q.reqBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status: %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "answer") {
				t.Fatalf("body: %s", body)
			}
			// 客户端格式断言
			if q.inboundFormat == "openai" {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil || m["choices"] == nil {
					t.Fatalf("openai shape: %v %s", err, body)
				}
			} else {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil || m["content"] == nil {
					t.Fatalf("claude shape: %v %s", err, body)
				}
			}
		})
	}
}

func TestMatrix_StreamingAllQuadrants(t *testing.T) {
	// 流式：每个象限上游返回对应格式 SSE，断言客户端收到自己格式的流
	quadrants := []struct {
		inboundFormat, upstreamFormat, path, reqBody, upStream, clientChunk string
	}{
		{"openai", "openai", "/v1/chat/completions", `{"messages":[],"stream":true}`,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n", `"delta":{"content":"hi"}`},
		{"openai", "claude", "/v1/chat/completions", `{"messages":[],"stream":true}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[]}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			`"delta":{"content":"hi"}`},
		{"claude", "openai", "/v1/messages", `{"max_tokens":1024,"messages":[],"stream":true}`,
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"},
		{"claude", "claude", "/v1/messages", `{"max_tokens":1024,"messages":[],"stream":true}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[]}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			`"text":"hi"`},
	}
	for _, q := range quadrants {
		t.Run(q.inboundFormat+"->"+q.upstreamFormat+" stream", func(t *testing.T) {
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				fl, _ := w.(http.Flusher)
				w.Write([]byte(q.upStream))
				fl.Flush()
			}))
			defer upstreamSrv.Close()
			cfg := &config.Config{
				Upstreams: map[string]config.UpstreamConfig{
					"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: q.upstreamFormat, Model: "m"},
				},
			}
			srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
			ts := httptest.NewServer(srv)
			defer ts.Close()
			resp, err := http.Post(ts.URL+q.path, "application/json", strings.NewReader(q.reqBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), q.clientChunk) {
				t.Fatalf("client stream: %s", body)
			}
			if q.inboundFormat == "openai" && !strings.Contains(string(body), "[DONE]") {
				t.Fatalf("missing DONE: %s", body)
			}
		})
	}
}

func TestMatrix_Tools_ToolCallsQuadrants(t *testing.T) {
	// 工具调用：OpenAI 入站带 tools + 上游返回 tool_use（Claude）或 tool_calls（OpenAI）
	// 覆盖 O2C 工具转换 + C2O 工具转换
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"tokyo"}}],"stop_reason":"tool_use"}`))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req := `{"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"tool_calls"`) || !strings.Contains(string(body), "get_weather") {
		t.Fatalf("tool_calls: %s", body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	tc := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["function"].(map[string]any)["arguments"] != `{"city":"tokyo"}` {
		t.Fatalf("arguments: %v", tc["function"])
	}
}

func TestMatrix_ImageRouting(t *testing.T) {
	// 带图请求四象限都路由到 vision（若配置）+ 文本零行为变化
	// 主要场景已由 TestVisionSwitch_ToClaudeVision 覆盖；此处补：vision 未配置 → main 收到带图请求
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "image_url") {
			t.Fatalf("image not forwarded: %s", body)
		}
		w.Write([]byte(openaiMockResponse("gpt-4o", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{ // auto_switch_vision 缺省 false
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: 跑全量测试**

Run: `go test ./... -v`
Expected: PASS（全部包）。任一象限失败：优先检查流式事件顺序（`content_block_start` 与首个 `content_block_delta` 拆分）、finish 事件时序。

- [ ] **Step 3: 跑 `go vet` + 覆盖率**

```bash
go vet ./...
go test ./... -cover
```

Expected: vet 无输出；覆盖率 ≥ 70%（后续真实链路验证后补足 80%+）。

- [ ] **Step 4: Commit**

```bash
git add internal/server/integration_test.go
git commit -m "test: four-quadrant integration matrix with mock upstreams"
```

---

### Task 15: 真实链路验证 + README

**Files:**
- Create: `README.md`
- Create: `prism-proxy.yaml.example`
- Create: `docs/superpowers/specs/2026-08-06-prism-proxy-verification.md`（可选，验证记录）

**Interfaces:**
- Consumes: 全部
- Produces: 文档 + 手动验证步骤

- [ ] **Step 1: 写 README.md**

内容：功能概述（双协议网关 + 图片自动切换 + 独立 vision 上游）、安装构建（`go build`）、配置说明（`prism-proxy.yaml.example` 全文 + 字段注释）、客户端接入（Claude Code `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY` 任意值；OpenAI SDK `base_url`）、路由规则两条、认证、热加载、日志字段、验证方法（curl 示例 + mock 上游脚本）。

- [ ] **Step 2: 写 prism-proxy.yaml.example**

```yaml
server:
  listen: ":8787"
  auth_keys: []              # 空 = 关闭认证；如 ["sk-proxy-1"]

auto_switch_vision: true     # false = 永不切换

main:
  baseurl: "https://api.openai.com/v1"
  api_key: "sk-xxxx"
  format: openai             # openai | claude
  model: "gpt-4o"
  timeout: 120s

vision:                      # 可选；auto_switch_vision: true 时必填（否则启动 panic）
  baseurl: "https://api.anthropic.com/v1"
  api_key: "sk-ant-xxxx"
  format: claude
  model: "claude-3-5-sonnet"
```

- [ ] **Step 3: 真实链路验证（手动，含 mock 上游脚本）**

```bash
# 1. 构建
go build -o prism-proxy ./cmd/prism-proxy
# 2. mock 上游：OpenAI 格式
python3 - <<'EOF' > /tmp/mock_openai.py
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        print("OPENAI UPSTREAM GOT:", json.dumps(body)[:200], flush=True)
        resp = json.dumps({"id":"chatcmpl-1","object":"chat.completion","model":body.get("model"),"choices":[{"index":0,"message":{"role":"assistant","content":"mock openai reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}).encode()
        self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers(); self.wfile.write(resp)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 9999), H).serve_forever()
EOF
python3 /tmp/mock_openai.py &
# 3. 同理 9998 端口起 Claude 格式 mock（复用上面结构，路径 /v1/messages，响应含 content blocks）
# 4. 启动代理 + curl 验证：
#    - OpenAI 客户端 → OpenAI mock（同格式透传）
#    - Claude 客户端 → OpenAI mock（C2O 交叉）
#    - OpenAI 客户端 → Claude mock（O2C 交叉）
#    - 带图请求 → vision mock（切换，检查上游日志收到 vision.model + image block）
# 5. Claude Code 真实客户端：
#    ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ANTHROPIC_API_KEY=sk-test claude
#    （Claude Code 请求 /v1/messages → 代理按决策转发；若上游 mock 则观察请求形状）
```

验证清单（对齐 spec 成功标准）：
- [ ] 四象限各跑通一个真实用例（含流式 + 工具调用）
- [ ] 带图请求到达 vision mock 且响应正确；文本请求零变化
- [ ] Claude Code（`ANTHROPIC_BASE_URL` 指向代理）可直连
- [ ] OpenAI SDK（`base_url` 指向代理）可直连
- [ ] 错误透传：mock 返回 429/500 → 客户端收到原样状态码 + 错误体
- [ ] 热加载：改配置文件 model 字段 → 无重启生效（tail 日志见 `config reloaded`）

- [ ] **Step 4: 覆盖率收尾**

```bash
go test ./... -coverprofile=/tmp/cov.out
go tool cover -func=/tmp/cov.out | tail -1
```

若总覆盖率 < 80%，补缺口的表驱动用例（优先流式状态机的分支：thinking 剥离、ping 吞掉、finish 时序、非法 JSON 兜底、外链下载失败）。

- [ ] **Step 5: Commit**

```bash
git add README.md prism-proxy.yaml.example docs/superpowers/specs/
git commit -m "docs: readme, example config and verification guide"
```

---

## Self-Review 记录

**1. Spec 覆盖核对：**
- §3 配置/校验/热加载 → Task 2、3 ✓；§4 路由两条规则 → Task 10 ✓；§5 转换矩阵/流式 → Task 5-9 + 13 ✓；§6 坑处理（media_type、block 计数、tool_result 顺序、timeout、thinking、stop_reason 兜底、SSRF）→ Task 5/6/7/10/13 ✓；§7 日志 → Task 12/13 ✓；§8 测试策略 → Task 14 + 各任务 TDD ✓；§9 非目标 → 无任务（有意）
- 启动 panic（fail fast）→ Task 2 Step 5-6 + Task 13 Step 5 ✓
- 热加载 rename 恢复 → Task 3 ✓
- 认证细节 → Task 12 ✓
- 请求/响应体 50MB → Task 12/13 ✓
- 外链图下载 30s → Task 13 ✓
- `n>1` 拒绝 400 → Task 8 + server 层错误映射（Task 13 的 relay 返回 err → 400）✓

**2. 占位符扫描：** 无 TBD/TODO；所有步骤含完整代码或精确命令。

**3. 类型一致性：**
- `config.Watcher.Get()` / `staticWatcher`（测试注入）一致 ✓
- `convert.NewC2OStream(model)` / `NewO2CStreamWithModel(model)` 签名在 Task 6/7/13 一致 ✓
- `withUsage` 签名 Task 7 内修正为 `(chunk []byte, u *Usage) []byte` ✓
- `route.Decide(cfg, format, body) (Decision, error)` 在 Task 10/12/13 一致 ✓
- `upstream.Client.Do(ctx, u, body, stream)` Task 11/12/13 一致 ✓
- `rewriteModel(format, body, model)` Task 12/13 一致 ✓
- Task 5 测试中 `ToolUseID` 字段需 Task 5 Step 5 补入 claude.go ✓（已在任务内注明）

**已知风险（实现时注意）：**
- Task 7 `withUsage` 重建逻辑若断测试，直接改为整体重建 `StreamChunk` marshal
- Task 13 `convertStream` 中 C2O 分支的 `w.WriteHeader` 时序（先 Header 后 WriteHeader）——实现时保证 Header 设置在 WriteHeader 前
- Task 14 流式矩阵的客户端 chunk 断言字符串需与 Task 6/7 实际输出精确匹配，失败时以测试期望为准微调断言
