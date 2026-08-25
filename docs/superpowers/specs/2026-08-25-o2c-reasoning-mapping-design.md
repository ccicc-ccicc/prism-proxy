# O2C 推理内容映射（reasoning_content → thinking）设计

日期：2026-08-25
状态：已批准（经 go-reviewer 设计评审，无 Critical，1 Important + 4 Minor 全部采纳）

## 背景

prism-proxy 是 OpenAI <-> Claude 双协议网关。线上配置：Claude Code（`/v1/messages`）→ prism-proxy → 网易 aigw。

问题：上游切换为 OpenAI 格式模型（`deepseek-v4-flash-ark-latest`，价格更低）时，上游返回的 `reasoning_content` 字段在 O2C 转换中被完全丢弃：

- 非流式：`ChatCompletionResponse.Choices[].Message` 内的 `reasoning_content`
- 流式：`StreamChunk.Choices[].Delta` 内的 `reasoning_content`（实测增量下发：`"We"` → `" need"` → `" answer"`…）

Claude Code 客户端因此看不到推理内容。已实测确认：

- 上游 OpenAI 格式两种形态均正常返回 `reasoning_content`（200）
- 请求方向无需改动：上游对历史消息里的 `reasoning_content` 实际忽略（实测"继续"请求未消费该字段），C2O 维持现有剥离行为

## 目标

1. O2C 方向完整映射：`reasoning_content` → Claude `thinking` 块（非流式 + 流式）
2. `signature` 字段省略（上游无签名；Claude Code 对第三方模型接受无签名 thinking，见风险）
3. 无 `reasoning_content` 时行为与现状完全一致（零回归）
4. 不做配置开关（方案 A，回退靠 git 回溯或上游 `format: claude` 切换）

## 设计

### 文件改动（4 个，全部在 `internal/convert/`）

#### 1. `openai.go` — 结构加字段

`ChatMessage` 加：

```go
ReasoningContent string `json:"reasoning_content,omitempty"`
```

`ChatMessage` 同时用于 C2O 出站请求序列化、O2C 响应 `Choice.Message`、O2C 流式 `StreamChoice.Delta`。已核实无副作用：全仓库仅 convert 包使用该结构；C2O 出站由 `ClaudeMessagesToOpenAI`（messages.go）构造新 `ChatMessage`，从不设置 `ReasoningContent`，`omitempty` 下出站请求不带该字段；`OpenAIMessagesToClaude` 只取 Role/Content/ToolCalls/ToolCallID，不会带出新字段。

#### 2. `response.go` — 非流式映射（评审修正 Important 1）

`OpenAIResponseToClaude` 中，thinking 块**在循环外**从 `resp.Choices[0].Message.ReasoningContent` 取一次（`TrimSpace` 非空才插入），循环内只追加 text/tool_calls。与现有"只取 `Choices[0]` 的 finish_reason/usage"（response.go:26-33）约定一致。

原因：若在循环内插入，上游违规返回多 choice 时会产生多个 thinking 块、且第二个 choice 的 thinking 落在 text 之后——违反 Anthropic 协议（thinking 只能一次且必须在 content 数组最前）。

块序：`thinking` → `text`* → `tool_use`*。

#### 3. `stream_o2c.go` — 流式状态机（评审修正 Important 2）

新增 `inThinking` 状态。统一修改 5 处块开关判断：

| 位置 | 现状 | 改为 |
|---|---|---|
| text 关块（stream_o2c.go:64） | `if s.inTool` | `if s.inTool || s.inThinking` |
| text 开块（stream_o2c.go:70） | `if !s.inText` | `if !s.inText && !s.inTool && !s.inThinking`（**必须**：否则 thinking 打开时收到 content 会开并行 text 块，协议非法） |
| tool_calls 关块（stream_o2c.go:78） | `if s.inText || s.inTool` | 加 `\|\| s.inThinking` |
| finish_reason 关块（stream_o2c.go:96） | `if s.inText \|\| s.inTool` | 加 `\|\| s.inThinking`（thinking 未闭合直接收 finish 的兜底） |
| `Finish()` 关块（stream_o2c.go:119） | `if s.inText \|\| s.inTool` | 加 `\|\| s.inThinking`（EOF 截断兜底） |

`delta.ReasoningContent` 非空（TrimSpace 后）→ 开 thinking 块发 `thinking_delta`；`delta.Content` 非空 → 先关 thinking 块、`blockIdx++`，再开 text 块。同一 chunk 内 reasoning + content 并存（异常防御）处理顺序：reasoning → text → tool_calls。

块索引序：thinking=0、text=1、tool_use=2，保持连续。

注释补充（评审 Minor 5）：流式 `reasoning_content` 按"逐 chunk 增量下发"处理（deepseek 系行为）；若未来接入全量下发上游，需先拼接再发射，避免重复拼接。

#### 4. `claude.go` — delta 结构加字段

`ClaudeDelta` 加（评审 Minor 3，顺带兼容）：

```go
Thinking  string `json:"thinking,omitempty"`
Signature string `json:"signature,omitempty"`
```

`ClaudeBlock` 已含 Thinking/Signature 字段（claude.go:47-48），tag 与真实 Anthropic 线格式一致（`{"type":"thinking","thinking":"...","signature":"..."}`），响应侧零改动。

## 数据流

- **非流式**：`content: [{type:"thinking",thinking:"..."}, {type:"text",text:"..."}]`（有 tool_use 时排文本后）
- **流式帧序列**：`content_block_start(thinking)` → `thinking_delta`* → `content_block_stop` → `content_block_start(text)` → `text_delta`* → … → `message_delta` → `message_stop`
- **无推理**：`reasoning_content` 为空/缺失 → 不发 thinking 块，帧序列与现状逐字节一致

## 边界与错误处理

- 空/空白 `reasoning_content`（`TrimSpace` 后为空）→ 不处理（评审 Minor 4）
- 纯 reasoning、text 空（推理被 max_tokens 截断）→ 仅 thinking 块
- 流式中途 finish / EOF → `Finish()` 幂等关闭 thinking 块
- `usage.reasoning_tokens`：`Usage` 结构无该字段，Go Unmarshal 静默忽略，行为正确（Claude usage 无对应字段），注释说明故意忽略
- 转换无新增错误路径；上游错误透传逻辑不动

## 测试计划

沿用现有表驱动风格（decodeEvent + 字符串断言）：

`stream_o2c_test.go`：
1. reasoning → text：帧序列 `[start(thinking,0), thinking_delta, stop(0), start(text,1), text_delta]`，索引 0/1 连续
2. reasoning → tool_calls：thinking idx 0 → tool_use idx 1
3. 纯 reasoning + `finish_reason=length`：thinking 关闭 + `message_delta{max_tokens}`，无 text 块
4. `Finish()` 时 thinking 未闭合 → `content_block_stop` + `message_stop`
5. 多 chunk reasoning 增量：连续 `thinking_delta` 不拆块
6. 同一 chunk reasoning + content 防御组合
7. `thinking_delta` 帧 `type="thinking_delta"` 形状断言
8. reasoning_content 空 → 现有全部测试通过（回归零差异）

`response_test.go`：
1. reasoning + text + tool_calls → blocks 顺序 `[thinking, text, tool_use]`
2. 纯 reasoning → 只 `[thinking]`
3. 多 choice 防御 → thinking 仅一次且在最前
4. C2O 序列化回归：Marshal 不含 ReasoningContent 的 ChatMessage，断言无 `reasoning_content` 键

验证命令：`go test -race ./...`

## 集成验证（本地，不碰生产）

1. 编译新二进制，本地 8799 端口起实例
2. 配置 `main: format: openai, model: deepseek-v4-flash-ark-latest`（指向网易 aigw）
3. curl `/v1/messages` 非流式 + 流式，断言 thinking 块出现、块序正确
4. 风险关卡：Claude Code 对无签名 thinking 的接受度 —— 本地验证通过后，生产重启后的第一个真实会话需确认思考可见且无报错

## 交付与回退

- commit → `docker build` → 推 `harbor.powerlaw.club/public/prism-proxy/prism-proxy:<git短sha>`
- **生产重启由用户执行**（拉新镜像 + 替换容器，`--restart always` 配置不动）
- 回退路径：上游切回 `format: claude`（现状配置）即恢复，无需换镜像；代码层面 git 回溯

## 风险

- **无签名 thinking 接受度（已知）**：Claude Code 对第三方模型通常接受无签名 thinking（可渲染、不可回传上下文）。若实测被拒：降级为剥离 thinking 只保 text，或评估占位签名。此为验证阶段第一关。
