# O2C 推理内容映射实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 prism-proxy 在 OpenAI 格式上游下，把 `reasoning_content` 完整映射为 Claude `thinking` 块（非流式 + 流式），使 Claude Code 走便宜模型时保留推理内容。

**Architecture:** 4 个文件改动，全部在 `internal/convert/`。`ChatMessage`/`ClaudeDelta` 加字段（`omitempty` 保证向后兼容），`OpenAIResponseToClaude` 在循环外从 `Choices[0]` 取 thinking 块前置，`O2CStream` 状态机加 `inThinking` 并在 5 处块开关判断统一扩展。参考设计文档：`docs/superpowers/specs/2026-08-25-o2c-reasoning-mapping-design.md`。

**Tech Stack:** Go 1.25.5+，标准库（encoding/json、strings），无新依赖。

## Global Constraints

- Go 版本 >= go.mod 声明（1.25.5+），`gofmt`/`goimports` 强制
- 所有 JSON 新字段必须 `omitempty`——出站请求与流量日志序列化复用同一结构
- `reasoning_content` 处理前必须 `strings.TrimSpace`，空白串不产生 thinking 块
- thinking 块只允许出现在 content 数组最前且仅一次（Anthropic 协议）；流式块索引必须连续
- 流式 `reasoning_content` 按"逐 chunk 增量下发"处理，不做跨 chunk 拼接
- `usage.reasoning_tokens` 故意忽略（Claude usage 无对应字段），加注释说明
- 无 `reasoning_content` 时行为与现状逐字节一致（零回归）
- commit 格式 `<type>: <description>`（attribution disabled），验证命令 `go test -race ./...`

---

### Task 1: 非流式 O2C reasoning 映射

**Files:**
- Modify: `internal/convert/openai.go:24-30`（ChatMessage 加字段）
- Modify: `internal/convert/response.go:7-35`（OpenAIResponseToClaude 映射）
- Test: `internal/convert/response_test.go`

**Interfaces:**
- Consumes: `ChatCompletionResponse.Choices[].Message.ReasoningContent`（上游响应字段，本任务在 `ChatMessage` 上定义）
- Produces: `ClaudeBlock{Type:"thinking", Thinking: rc}`（`ClaudeBlock` 已含 Thinking 字段，见 `claude.go:47-48`）；供 Task 2 复用的字段：`ChatMessage.ReasoningContent string`

- [ ] **Step 1: 写失败测试**

在 `internal/convert/response_test.go` 追加（文件已 import `strings`/`testing`，需加 `encoding/json`）：

```go
func TestOpenAIResponseToClaude_ReasoningMapped(t *testing.T) {
	resp := &ChatCompletionResponse{
		Choices: []ResponseChoice{{Message: ChatMessage{
			Content:          "hi",
			ReasoningContent: "think step by step",
			ToolCalls:        []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: `{"x":1}`}}},
		}, FinishReason: "tool_calls"}},
	}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 3 {
		t.Fatalf("want 3 blocks, got %d: %+v", len(out.Content), out.Content)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "think step by step" {
		t.Fatalf("first block must be thinking: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "hi" {
		t.Fatalf("second block must be text: %+v", out.Content[1])
	}
	if out.Content[2].Type != "tool_use" {
		t.Fatalf("third block must be tool_use: %+v", out.Content[2])
	}
}

func TestOpenAIResponseToClaude_ReasoningOnly(t *testing.T) {
	resp := &ChatCompletionResponse{Choices: []ResponseChoice{{Message: ChatMessage{Content: "", ReasoningContent: "only thinking"}}}}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "thinking" || out.Content[0].Thinking != "only thinking" {
		t.Fatalf("want only thinking: %+v", out.Content)
	}
}

// Important 1: 多 choice 防御——thinking 仅一次且在最前（取自 Choices[0]）
func TestOpenAIResponseToClaude_MultiChoiceThinkingOnce(t *testing.T) {
	resp := &ChatCompletionResponse{
		Choices: []ResponseChoice{
			{Message: ChatMessage{Content: "a", ReasoningContent: "r1"}},
			{Message: ChatMessage{Content: "b", ReasoningContent: "r2"}},
		},
	}
	out, err := OpenAIResponseToClaude(resp, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 3 {
		t.Fatalf("want 3 blocks: %+v", out.Content)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "r1" {
		t.Fatalf("thinking once first: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[2].Type != "text" {
		t.Fatalf("texts follow: %+v", out.Content)
	}
}

// 回归：不含 reasoning 的 ChatMessage 序列化不得带 reasoning_content 键（C2O 出站安全）
func TestChatMessageMarshalNoReasoningContent(t *testing.T) {
	b, err := json.Marshal(ChatMessage{Role: "assistant", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reasoning_content") {
		t.Fatalf("unexpected reasoning_content key: %s", b)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run 'ReasoningMapped|ReasoningOnly|MultiChoiceThinkingOnce|MarshalNoReasoningContent'`
Expected: FAIL，编译错误 `ChatMessage has no field ReasoningContent`

- [ ] **Step 3: 最小实现**

`internal/convert/openai.go` 的 `ChatMessage` 加字段（`ToolCalls` 前一行）：

```go
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}
```

`internal/convert/response.go` 的 `OpenAIResponseToClaude`，在 `var blocks []ClaudeBlock` 之后、`for _, ch := range resp.Choices` 之前插入：

```go
	if len(resp.Choices) > 0 {
		// thinking 只在最前且仅一次（取自 Choices[0]，与 finish_reason/usage 的
		// 约定一致）。usage.reasoning_tokens 故意忽略：Claude usage 无对应字段。
		if rc := strings.TrimSpace(resp.Choices[0].Message.ReasoningContent); rc != "" {
			blocks = append(blocks, ClaudeBlock{Type: "thinking", Thinking: rc})
		}
	}
```

`response.go` 已 import `strings`，无需新增。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/convert/ -run 'ReasoningMapped|ReasoningOnly|MultiChoiceThinkingOnce|MarshalNoReasoningContent|TestOpenAIResponseToClaude' -v`
Expected: 全部 PASS（含既有 `TestOpenAIResponseToClaude`、`TestOpenAIResponseToClaude_EmptyChoices`）

- [ ] **Step 5: Commit**

```bash
git add internal/convert/openai.go internal/convert/response.go internal/convert/response_test.go
git commit -m "feat: map reasoning_content to thinking block in O2C non-streaming response"
```

---

### Task 2: 流式 O2C reasoning 映射

**Files:**
- Modify: `internal/convert/claude.go:107-113`（ClaudeDelta 加字段）
- Modify: `internal/convert/stream_o2c.go`（状态机 + 5 处块开关判断）
- Test: `internal/convert/stream_o2c_test.go`

**Interfaces:**
- Consumes: `ChatMessage.ReasoningContent`（Task 1 定义）、`O2CStream.Write([]byte) ([][]byte, error)`、`O2CStream.Finish() [][]byte`（既有签名不变）
- Produces: `ClaudeDelta.Thinking string` / `ClaudeDelta.Signature string`（thinking_delta 帧负载）

- [ ] **Step 1: 写失败测试**

在 `internal/convert/stream_o2c_test.go` 末尾追加（复用既有 `decodeEvent` 辅助，风格同 `TestO2CStream_ToolThenText`）：

```go
// reasoning → text：thinking idx 0 → text idx 1，索引连续
func TestO2CStream_ReasoningThenText(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"We"},"finish_reason":null}]}`))
	if len(out) != 2 {
		t.Fatalf("reasoning start frames: %d (%s)", len(out), out)
	}
	start := decodeEvent(t, out[0])
	if start.ContentBlock == nil || start.ContentBlock.Type != "thinking" || start.Index != 0 {
		t.Fatalf("thinking block must be index 0: %+v", start)
	}
	delta := decodeEvent(t, out[1])
	if delta.Delta == nil || delta.Delta.Type != "thinking_delta" || delta.Delta.Thinking != "We" {
		t.Fatalf("thinking delta: %+v", delta.Delta)
	}
	// text 到达 → 关 thinking，开 text idx 1
	out, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`))
	if len(out) != 3 {
		t.Fatalf("reasoning-then-text frames: %d (%s)", len(out), out)
	}
	if !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("thinking must close: %s", out[0])
	}
	textStart := decodeEvent(t, out[1])
	if textStart.ContentBlock == nil || textStart.ContentBlock.Type != "text" || textStart.Index != 1 {
		t.Fatalf("text block must be index 1: %+v", textStart)
	}
}

// 纯 reasoning + length finish：thinking 关闭 + message_delta{max_tokens}，无 text 块
func TestO2CStream_ReasoningOnlyLengthFinish(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"deep"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`))
	if len(out) != 2 || !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("thinking must close before message_delta: %s", out)
	}
	if !strings.Contains(string(out[1]), "\"stop_reason\":\"max_tokens\"") {
		t.Fatalf("finish: %s", out[1])
	}
}

// Finish() 时 thinking 未闭合 → content_block_stop + message_stop
func TestO2CStream_FinishClosesThinking(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"deep"},"finish_reason":null}]}`))
	fin := s.Finish()
	if len(fin) != 2 || !strings.Contains(string(fin[0]), "content_block_stop") || !strings.Contains(string(fin[1]), "message_stop") {
		t.Fatalf("finish must close thinking: %s", fin)
	}
}

// 多 chunk 增量 → 连续 thinking_delta 不拆块
func TestO2CStream_ReasoningContinuationNoSplit(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"a"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"b"},"finish_reason":null}]}`))
	if len(out) != 1 {
		t.Fatalf("continuation frames: %d (%s)", len(out), out)
	}
	ev := decodeEvent(t, out[0])
	if ev.Index != 0 || ev.Delta == nil || ev.Delta.Type != "thinking_delta" || ev.Delta.Thinking != "b" {
		t.Fatalf("continuation delta: %+v", ev)
	}
}

// reasoning → tool_calls：thinking idx 0 → tool_use idx 1
func TestO2CStream_ReasoningThenTool(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`))
	if len(out) != 2 || !strings.Contains(string(out[0]), "content_block_stop") {
		t.Fatalf("thinking must close before tool start: %s", out)
	}
	toolStart := decodeEvent(t, out[1])
	if toolStart.ContentBlock == nil || toolStart.ContentBlock.Type != "tool_use" || toolStart.Index != 1 {
		t.Fatalf("tool_use must be index 1 after thinking: %+v", toolStart)
	}
}

// 同一 chunk reasoning + content（异常防御）：reasoning 先、text 后
func TestO2CStream_ReasoningAndContentSameChunk(t *testing.T) {
	s := NewO2CStream()
	_, _ = s.Write([]byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`))
	out, _ := s.Write([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"r","content":"c"},"finish_reason":null}]}`))
	// [start(thinking,0), thinking_delta, stop(0), start(text,1), text_delta]
	if len(out) != 5 {
		t.Fatalf("same-chunk frames: %d (%s)", len(out), out)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/convert/ -run 'ReasoningThenText|ReasoningThenTool|ReasoningOnlyLengthFinish|FinishClosesThinking|ReasoningContinuationNoSplit|ReasoningAndContentSameChunk'`
Expected: FAIL（`ClaudeDelta` 无 Thinking 字段编译失败；reasoning delta 被忽略断言不通过）

- [ ] **Step 3: 最小实现**

`internal/convert/claude.go` 的 `ClaudeDelta` 加字段：

```go
type ClaudeDelta struct {
	Type         string  `json:"type,omitempty"`
	Text         string  `json:"text,omitempty"`
	Thinking     string  `json:"thinking,omitempty"`
	Signature    string  `json:"signature,omitempty"`
	PartialJSON  string  `json:"partial_json,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}
```

`internal/convert/stream_o2c.go`：

1. import 加 `strings`（当前为 `encoding/json`、`fmt`）。
2. 结构体加字段：

```go
type O2CStream struct {
	id         string
	model      string
	started    bool
	blockIdx   int
	inText     bool
	inTool     bool
	inThinking bool
	sentStop   bool
	done       bool
}
```

3. `Write` 中 `delta := chunk.Choices[0].Delta` 之后、text 处理之前插入 reasoning 分支：

```go
	// reasoning_content 逐 chunk 增量下发（deepseek 系行为）；若上游改为
	// 全量下发，需先拼接再发射，避免重复拼接。
	if rc := strings.TrimSpace(delta.ReasoningContent); rc != "" {
		if s.inText || s.inTool {
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText, s.inTool = false, false
			s.blockIdx++
		}
		if !s.inThinking {
			s.inThinking = true
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "thinking"}}))
		}
		frames = append(frames, s.frame(EventContentBlockDelta, StreamEvent{Index: s.blockIdx, Delta: &ClaudeDelta{Type: "thinking_delta", Thinking: rc}}))
	}
```

4. text 分支关块/开块判断扩展（现有 64/70 行）：

```go
		if s.inTool || s.inThinking {
			// 块切换（tool_use/thinking → text）：先关当前块并递增 index
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText, s.inTool, s.inThinking = false, false, false
			s.blockIdx++
		}
		if !s.inText && !s.inTool && !s.inThinking {
			s.inText = true
			frames = append(frames, s.frame(EventContentBlockStart, StreamEvent{Index: s.blockIdx, ContentBlock: &ClaudeBlock{Type: "text"}}))
		}
```

5. tool_calls 关块判断（现有 78 行）扩展：

```go
			if s.inText || s.inTool || s.inThinking {
				frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
				s.inText, s.inTool, s.inThinking = false, false, false
				s.blockIdx++
			}
```

6. finish_reason 关块（现有 96 行）与 `Finish()` 关块（现有 119 行）扩展：

```go
		if s.inText || s.inTool || s.inThinking {
			frames = append(frames, s.frame(EventContentBlockStop, StreamEvent{Index: s.blockIdx}))
			s.inText, s.inTool, s.inThinking = false, false, false
		}
```

（两处相同替换。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test -race ./internal/convert/`
Expected: 全部 PASS（新增 5 个 + 既有全部用例）

- [ ] **Step 5: Commit**

```bash
git add internal/convert/claude.go internal/convert/stream_o2c.go internal/convert/stream_o2c_test.go
git commit -m "feat: map reasoning_content to thinking stream in O2C"
```

---

### Task 3: 全量回归 + 本地集成验证

**Files:**
- 无代码改动；创建临时配置 `/tmp/prism-test.yaml`（不入库）
- Test: 全量单测 + 本地实例 curl

**Interfaces:**
- Consumes: Task 1/2 产物（新二进制）

- [ ] **Step 1: 全量单测回归**

Run: `go test -race ./...`
Expected: 全部 PASS

- [ ] **Step 2: 编译测试二进制**

Run: `go build -o /tmp/prism-proxy-test ./cmd/prism-proxy`
Expected: 成功，无输出

- [ ] **Step 3: 写本地验证配置**

从 `~/.prism-proxy/settings.yaml` 取 api_key（不落盘明文以外部分），创建 `/tmp/prism-test.yaml`：

```yaml
server:
  listen: ":8799"
  auth_keys: []
main:
  baseurl: "https://aigw.netease.com/v1"
  api_key: "<~/.prism-proxy/settings.yaml 中 main.api_key>"
  format: openai
  model: "deepseek-v4-flash-ark-latest"
  auth: bearer
```

（无 vision 段——本任务不测图片切换。）

- [ ] **Step 4: 起本地实例**

Run: `/tmp/prism-proxy-test serve --config /tmp/prism-test.yaml`（后台运行）
Expected: 启动日志无报错

- [ ] **Step 5: 非流式验证**

Run:
```bash
curl -s http://127.0.0.1:8799/v1/messages -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" -d '{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":"1+1=?"}]}'
```
Expected: 响应 `content` 数组第一块 `{"type":"thinking","thinking":"..."}`（无 signature 字段），随后 text 块

- [ ] **Step 6: 流式验证**

Run:
```bash
curl -sN http://127.0.0.1:8799/v1/messages -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" -d '{"model":"ignored","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"1+1=?"}]}' | grep -c "thinking_delta"
```
Expected: 输出 > 0（出现 thinking_delta 帧）；且 `content_block_start` 中 thinking 块 index 0、text 块 index 1 连续

- [ ] **Step 7: 清理**

Run: 停止测试实例，删除 `/tmp/prism-proxy-test`、`/tmp/prism-test.yaml`
Expected: 无残留进程（`lsof -i :8799` 为空）

- [ ] **Step 8: 汇报验证结果**

向用户汇报：单测全绿 + 本地实例非流式/流式 thinking 块验证通过/失败详情。**不执行生产任何操作。**

---

### Task 4: 构建并推送镜像

**Files:**
- 无代码改动；交付物为 harbor 镜像

**Interfaces:**
- Consumes: Task 1-3 全部通过（含用户对验证结果的确认）

- [ ] **Step 1: 确认 git 状态干净**

Run: `git status --short`
Expected: 空（Task 1/2 已提交，无未提交改动）

- [ ] **Step 2: 构建镜像**

Run: `docker build -t harbor.powerlaw.club/public/prism-proxy/prism-proxy:$(git rev-parse --short HEAD) .`
Expected: 构建成功

- [ ] **Step 3: 推送镜像**

Run: `docker push harbor.powerlaw.club/public/prism-proxy/prism-proxy:$(git rev-parse --short HEAD)`
Expected: 推送成功

- [ ] **Step 4: 通知用户**

输出重启指引（**由用户执行，不自动重启生产**）：

```bash
# 10.244.138.168 生产实例
docker pull harbor.powerlaw.club/public/prism-proxy/prism-proxy:<git短sha>
docker stop prism-proxy && docker rm prism-proxy
docker run -d --name prism-proxy --restart always \
  -p 8787:8787 \
  -v ~/.prism-proxy:/root/.prism-proxy \
  harbor.powerlaw.club/public/prism-proxy/prism-proxy:<git短sha>
```

同时提醒：生产配置 `main.format` 需改为 `openai`、`model` 改为 `deepseek-v4-flash-ark-latest`（vision 段如需保留可不动）。重启后第一个真实会话确认思考可见且无报错；若 Claude Code 拒绝无签名 thinking（已知风险），回退路径为上游切回 `format: claude`。
