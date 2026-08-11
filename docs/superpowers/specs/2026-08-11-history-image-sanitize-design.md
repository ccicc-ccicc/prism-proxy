# 设计文档：历史图片智能替换（History Image Sanitize）

日期：2026-08-11
状态：已批准

## 1. 背景与问题

`prism-proxy` 是本地无状态 LLM API 代理（OpenAI <-> Claude 双协议网关）。`auto_switch_vision: true` 时，入站请求含图自动路由到独立的 `vision` 上游，文本请求走 `main`。

**问题场景**：客户端是 Claude Code CLI。CLI 在同一会话内每轮请求都会重放完整消息历史（API 无状态，服务端不存消息），历史中的图片 content block（含 `tool_result` 内嵌截图）原样保留，直到会话压缩（auto-compact）才被摘要化丢弃。

当前 `route.RequestHasImage` **遍历全部消息**检测图片。因此：

- 会话第 1 轮传图后，第 2、3、4…轮请求历史仍含图块 → **每轮都触发 vision 切换** → 每轮都走更贵的 vision 模型（成本问题）。
- 若走 `main`（纯文本模型），历史图块发给不支持图的模型 → 上游 400 报错，会话中断（可用性问题）。

**核心约束**：`main` 上游是纯文本模型；客户端重放历史图片直到压缩。

## 2. 目标

1. 只有**新一轮传图**（末尾用户侧消息 run 含图）才切 vision。
2. 纯文本轮次走 `main`，历史图块替换为轻量标记，模型通过历史中 assistant 的解析文本保持语义理解。
3. 不静默丢图：最新消息的图永不脱敏（要么切 vision，要么原样走 main 显式报错）。
4. 无状态实现：不引入会话 ID、内存缓存等跨请求状态。

## 3. 方案

### 3.1 检测语义收窄

`route.RequestHasImage` 改为只检测**末尾"用户侧消息 run"**，更名 `LatestUserRunHasImage`：

- **锚点定义（统一两种格式）**：从消息序列末尾向前，取中间无 assistant 消息打断的连续"用户侧"消息段（run）：
  - Claude 格式：末尾连续的 `role == "user"` 消息段（`tool_result` 由 user 消息携带；`role == "assistant"` 消息打断 run）。
  - OpenAI 格式：末尾连续的 `role == "user"` 或 `role == "tool"` 消息段（工具结果由 tool role 消息携带；`role == "assistant"` 消息打断 run）。
  - 序列以 assistant 消息结尾时，run 为其前最后一段连续用户侧消息。
- **检测**：run 内任一消息含图 → true（Claude 递归检查 content，含 `tool_result.content` 内嵌 image block；OpenAI 检查 content 数组中 `image_url` part）。
- **边界**：run 为空（消息序列无 user/tool 消息，异常请求）→ 返回 false。
- 单消息请求（无历史重放）行为与全量检测等价，无差异。

> **为何用 run 而非单条消息**：客户端可能把同一轮内容拆成多条连续 user 消息（首条带图、末条纯文本）。若只查最后一条，新图被误判为历史，走 main 脱敏后本轮静默丢图——检测必须覆盖整段用户侧 run。

### 3.2 历史图片替换（Sanitize）

新增 `route.SanitizeHistoryImages(format, body []byte) ([]byte, bool, error)`：

- 遍历边界（与 §3.1 锚点一致，随格式）：**run 之前的所有消息**（run 即末尾用户侧消息段；run 为空 → 脱敏 no-op，原样返回、sanitized=false）。
- **实现策略（重要）**：必须 **map 基遍历**（`[]any` / `map[string]any`），**禁止 typed struct 往返**。原因：typed 反序列化-再序列化（如 `MessagesRequest`/`ClaudeBlock`）会静默丢弃结构体未定义的字段——Claude Code 在块级携带 `cache_control`（prompt caching）、消息级携带 `metadata` 等，丢弃会破坏 main 上游的 prompt caching（每轮重新计费，与成本目标直接冲突）。map 基遍历只替换图片块，其余键原样保留（含 `tool_result` 的 `is_error`/`tool_use_id`）。与现有 `rewriteModel` 的 map 基做法一致。
- Claude 格式：
  - `image` block → `{"type": "text", "text": "[image: analyzed in previous reply]"}`
  - `tool_result` 的 content 数组内嵌 `image` block 递归替换
- OpenAI 格式：
  - content 数组中 `image_url` part → `{"type": "text", "text": "[image: analyzed in previous reply]"}`
- 同消息的 text part/block 原样保留。
- **回退规则**：含图消息之后**向后扫描**到下一个含**非空 text 块**的 assistant 消息（可跨过中间的 `tool_use`/user 工具循环消息——工具循环中截图后的紧邻 assistant 常是无文本的 `tool_use`，解析文本在更靠后的 assistant 消息里），扫到序列末尾仍无 → 标记降级为 `[image omitted]`。
- 返回：替换后的 body、是否发生替换（bool）、错误（解析失败 → error，与 `Decide` 一致走 400）。

**语义原理**：模型对图片的解析文本（assistant 回复）**本来就在请求历史中**，原位保留即可——模型读到"user 发了图" + "assistant 分析了图"，语义完整。图片块无需复制解析文本，只需替换为短标记，token 从 base64（~1-2k token）降到 ~10 token。

### 3.3 转发管线挂接

`internal/server/server.go` 的 `ServeHTTP`，在 `route.Decide` 之后、`relay` 之前：

```go
if decision.Upstream == "main" && cfg.AutoSwitchVision {
    body, sanitized, err = route.SanitizeHistoryImages(format, body)
    if err != nil {
        writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error())
        return
    }
    decision.ImagesSanitized = sanitized
}
```

- **切 vision 时不调用**：vision 模型能处理历史图，保留完整上下文。
- **`auto_switch_vision: false` 不调用**：完全原样转发（现状语义，用户全权控制）。
- 无 vision 上游配置时落回 main：最新图原样（上游报错 = 显式提示配置问题），历史图仍脱敏（`AutoSwitchVision` 为 true 即脱敏）。

## 4. 行为矩阵

`auto_switch_vision: true`：

| 末尾用户侧消息 run | 路由 | 历史图处理 |
|---|---|---|
| 含图 | vision | 保留（body 原样） |
| 无图，历史含图 | main | 替换为短标记，assistant 解析文本原位保留 |
| 无图，历史无图 | main | 原样 |

边界：

- `auto_switch_vision: false` → 完全原样，任何图不处理。
- 无 vision 上游 → 落回 main，最新图不脱敏（显式报错），历史图脱敏。**注意**：`config.Validate` 对 `auto_switch_vision: true` 缺 vision 上游是启动 fail-fast（热加载失败保留旧配置），此路径仅测试直接构造 config 或热加载竞态下可达——防御路径，实现时不为它加额外分支。

## 5. 代码改动清单

| 文件 | 改动 |
|---|---|
| `internal/route/route.go` | `LatestUserRunHasImage`（改语义+更名）、`SanitizeHistoryImages`（新增，map 基实现）、`Decision` 加 `ImagesSanitized bool` 字段 |
| `internal/server/server.go` | `ServeHTTP` 挂接脱敏；`log()` 输出 `images_sanitized`；`image_detected` 日志语义随 `HasImage` 更名变化（见下） |

**日志语义（随检测收窄变化）**：`image_detected`（server.go `log()` 中 `HasImage && !VisionSwitch` 分支）从"全量含图"变为"末尾用户侧 run 含图"。`auto_switch_vision: false` 且历史含图走 main 时：既无 `image_detected` 也无 `images_sanitized`，上游 400 原因不可见——接受此观测空白（false 模式为完全原样），不为此加独立标记。
| `README.md` | 更新 `auto_switch_vision` 语义说明 |
| 测试 | 见 §6 |

## 6. 测试计划

- `internal/route/route_test.go`：
  - `LatestUserRunHasImage`：末尾 run 含图 → true；历史含图但 run 无图 → false；tool_result 内嵌图 → true；OpenAI `role=tool` 消息含图 → true；**连续 user 消息 run（首条带图、末条纯文本）→ true**；**无 user/tool 消息 → false**；OpenAI 最后一条为 assistant 消息（run 为其前段）。
  - `SanitizeHistoryImages`（表驱动）：Claude image block、Claude tool_result 内嵌、OpenAI image_url、**工具循环回退（`[user(tool_result+图), assistant(tool_use 无文本), assistant(含文本)]` → analyzed 标记）**、无含文本 assistant 消息 → `[image omitted]`、run 内消息不动、混合 text 保留、**map 基往返保留未知键（块级 `cache_control`、`tool_result` 的 `is_error`/`tool_use_id`）**、**无图时 no-op 幂等（sanitized=false、body 逐字节一致）**、解析失败返回 error、**run 为空 → no-op**。
- `internal/server/integration_test.go`：
  - 历史图 + 纯文本轮次 → main 上游收到**无图** body。
  - 最新图轮次 → vision 上游收到**原样** body。
  - **交叉格式**：OpenAI 入站→Claude 上游 main、Claude 入站→OpenAI 上游 main，脱敏 body 经转换管线后无图。
  - **脱敏与 `rewriteModel` 共存**：走 main 时 model 改写与历史图脱敏同时生效。
  - `auto_switch_vision: false` → body 原样转发（现状回归）。
- 现有 `RequestHasImage` 相关测试同步更名/语义更新。
- 验证：`go test -race ./...` + `go test -cover ./...`（保持 80%+ 覆盖率）。

## 7. 错误处理

- `SanitizeHistoryImages` 解析失败 → error → 400（与 `Decide` 错误信封一致，`writeError` 已按入站格式回写）。
- 上游报错原样透传（现状，spec 已有行为）。

## 8. 不做的事（YAGNI）

- 不引入跨请求会话状态（无会话 ID 可靠来源，且内存缓存多实例不可共享）。
- 不复制解析文本进图片块位置（token 浪费，文本已在历史中）。
- 不加新配置项（行为由现有 `auto_switch_vision` 门控）。
- 不改 vision 轮次行为（历史图保留，上下文完整）。

## 9. 风险

- 语义损失仅限"纯文本轮次模型看不到历史图"，但 assistant 解析文本仍在历史中，问答可继续；用户在新会话重新传图即恢复完整视觉上下文。
- `LatestUserRunHasImage` 语义变更对重放历史的客户端是新行为（预期），对单消息客户端无差异。连续 user 消息场景（首条带图、末条纯文本）由 run 锚点覆盖，新图不会被误判为历史。
- 脱敏必须 map 基实现（见 §3.2）：若误用 typed 往返会丢 `cache_control`，破坏 main 上游 prompt caching——实现计划中列为强制要求。
