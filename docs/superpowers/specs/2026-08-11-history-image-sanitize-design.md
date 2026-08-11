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

1. 只有**新一轮传图**（最后一条 user 消息含图）才切 vision。
2. 纯文本轮次走 `main`，历史图块替换为轻量标记，模型通过历史中 assistant 的解析文本保持语义理解。
3. 不静默丢图：最新消息的图永不脱敏（要么切 vision，要么原样走 main 显式报错）。
4. 无状态实现：不引入会话 ID、内存缓存等跨请求状态。

## 3. 方案

### 3.1 检测语义收窄

`route.RequestHasImage` 改为只检测**最后一条 user 消息**，更名 `LatestMessageHasImage`：

- Claude 格式：遍历 `MessagesRequest.Messages`，定位最后一条 `role == "user"` 的消息，递归检查其 content（含 `tool_result.content` 内嵌 image block）。
- OpenAI 格式：只检查 messages 数组最后一条消息的 content 数组（`image_url` part）。
- 单消息请求（无历史重放）行为与全量检测等价，无差异。

### 3.2 历史图片替换（Sanitize）

新增 `route.SanitizeHistoryImages(format, body []byte) ([]byte, bool, error)`：

- 遍历边界（与 §3.1 检测语义一致，随格式）：
  - Claude 格式：**除最后一条 `role == "user"` 消息外**的所有消息（tool_result 由 user 消息携带，最后一条 user 消息即"用户最后发出的内容"）。
  - OpenAI 格式：**除最后一条消息外**的所有消息（工具结果由 tool role 消息携带，最后一条消息即等价语义）。
- Claude 格式：
  - `image` block → `{"type": "text", "text": "[image: analyzed in previous reply]"}`
  - `tool_result` 的 content 数组内嵌 `image` block 递归替换
- OpenAI 格式：
  - content 数组中 `image_url` part → `{"type": "text", "text": "[image: analyzed in previous reply]"}`
- 同消息的 text part/block 原样保留。
- **回退规则**：含图消息之后**紧邻的下一条 assistant 消息**（中间不含其他 user 消息）不存在文本回复（异常历史）时，标记降级为 `[image omitted]`。
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

| 最后一条 user 消息 | 路由 | 历史图处理 |
|---|---|---|
| 含图 | vision | 保留（body 原样） |
| 无图，历史含图 | main | 替换为短标记，assistant 解析文本原位保留 |
| 无图，历史无图 | main | 原样 |

边界：

- `auto_switch_vision: false` → 完全原样，任何图不处理。
- 无 vision 上游 → 落回 main，最新图不脱敏（显式报错），历史图脱敏。

## 5. 代码改动清单

| 文件 | 改动 |
|---|---|
| `internal/route/route.go` | `LatestMessageHasImage`（改语义+更名）、`SanitizeHistoryImages`（新增）、`Decision` 加 `ImagesSanitized bool` 字段 |
| `internal/server/server.go` | `ServeHTTP` 挂接脱敏；`log()` 输出 `images_sanitized` |
| `README.md` | 更新 `auto_switch_vision` 语义说明 |
| 测试 | 见 §6 |

## 6. 测试计划

- `internal/route/route_test.go`：
  - `LatestMessageHasImage`：最后一条 user 消息含图 → true；历史含图但最新无图 → false；tool_result 内嵌图 → true；OpenAI 格式最后一条消息；历史图不触发。
  - `SanitizeHistoryImages`（表驱动）：Claude image block、Claude tool_result 内嵌、OpenAI image_url、无 assistant 回复回退 `[image omitted]`、最新消息不动、混合 text 保留、解析失败返回 error。
- `internal/server/integration_test.go`：
  - 历史图 + 纯文本轮次 → main 上游收到**无图** body。
  - 最新图轮次 → vision 上游收到**原样** body。
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
- `LatestMessageHasImage` 语义变更对重放历史的客户端是新行为（预期），对单消息客户端无差异。
