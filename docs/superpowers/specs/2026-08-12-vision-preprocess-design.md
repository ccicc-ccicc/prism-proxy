# 设计文档：Vision 预处理（Vision Preprocess）

日期：2026-08-12
状态：已批准

## 1. 背景与问题

`auto_switch_vision: true` 时，最新轮次含图请求整请求切到 vision 上游（gpt-5-nano）。生产实测（traffic log request 5970d61a）：**单张 486KB base64 截图 ≈ 12 万 token**，加上历史文本（142k 字符 ≈ 3.6 万 token）超出 gpt-5-nano 上下文窗口 → `context_length_exceeded` 400。

根因：**超大图 + 小窗口 vision 模型 + 历史文本累积**——切 vision 时历史与图一起发给小窗口模型。

## 2. 目标

1. **结构性解决超限**：vision 上游只收图（永不收历史）、main 上游只收文本（永不收图）。
2. **保持语义**：图片经 vision 解析为文本后注入请求，main 模型通过解析文本理解图。
3. **可回退**：`vision_preprocess` 配置开关，false = 现有行为（整请求切 vision），问题时可切换回原状态。

## 3. 方案

### 3.1 配置

```yaml
vision_preprocess: true   # 默认 false
```

- `false`（默认）：现有行为——最新 run 含图 → 整请求切 vision 上游。
- `true`：预处理模式——最新 run 含图 → vision 单独解析图片 → 解析文本替换 → main 上游。

### 3.2 流程（vision_preprocess: true）

```
原请求（历史 + 最新 run 含图）
  │
  ├─ ① 提取最新 run 内 image 块（base64 + media_type）+ 用户文本（run 内 user 消息 text，排除 tool_result）
  │     run 边界与 SanitizeHistoryImages / LatestUserRunHasImage 完全一致：
  │     末尾连续用户侧消息段，任何 assistant（含 tool_use）打断
  │     （即只提取"最后一段用户侧消息"的图 = 当前活跃内容；工具循环中间的图属历史，走脱敏）
  │
  ├─ ② 构造 vision 预处理请求（vision 上游格式，非流式，max_tokens 必设）：
  │     messages: [user content: [image_url part..., text: 固定指令 + 用户上下文]]
  │     多图合并一次请求；只发图 + 短 prompt（永不超限）
  │
  ├─ ③ 调 vision 上游 → 解析响应文本
  │
  ├─ ④ 替换：run 内 image 块 → {"type":"text","text":"[图片内容: <解析文本>]"}
  │
  ├─ ⑤ 历史图脱敏（现有 SanitizeHistoryImages）
  │
  └─ ⑥ 无图请求 → main 上游（relay 现有逻辑）
```

### 3.3 解析 Prompt

```
固定指令："请依次详细描述每一张图片的内容：所有可见文本、界面元素、数据、状态。用中文回答。"
用户上下文（run 内存在 user 文本时追加）："结合用户问题「<文本>」，重点描述图中相关内容。"
```

用户文本提取：最新 run 内 `role == "user"` 消息的 text 块/字符串 content，**排除 tool_result 块**（工具输出非用户输入）；无用户文本则只发固定指令。

### 3.4 错误处理与状态码

- **请求侧错误（提取/解析失败、构造失败、占位缺失）→ 400**（`writeError`，显式失败，不静默丢图）。
- **vision 上游调用失败（非 2xx / 网络 / 超时）→ 502**（与现有 `forward` 的上游失败语义一致；错误信封含上游错误信息）。
- 单图解码/提取失败 → 该图替换为 `[image omitted]`，其余图继续预处理。
- **URL 图片来源**（claude `source.type=url` / openai 非 data URL）：预处理模式下降级 `[image omitted]`（当前用户场景为 base64 内嵌截图；URL 下载留待后续，文档化不静默）。
- 多图合并请求若 vision 上游超限 → 502（上游错误原样语义）。

### 3.5 观测

- 日志字段：`vision_preprocess: true`（发生预处理）、`vision_preprocess_ms`（预处理耗时）。
- 预处理请求（`NeedPreprocess=true`）不输出 `image_detected`（避免语义冗余，仅输出 `vision_preprocess`）。

### 3.6 不做的事（YAGNI）

- 不做解析结果缓存（无状态，每请求独立预处理；同会话内历史图走现有脱敏短标记，不重复预处理）。
- 不做图片压缩（预处理后 main 无图、vision 无历史，超限结构性消除；压缩留待图本身超单图窗口时再议）。
- 不改 vision 上游格式（复用现有 vision 配置：openai/claude 均可）。
- 不下载 URL 图片来源（预处理模式下降级 omitted，见 §3.4）。

## 4. 行为矩阵

| vision_preprocess | 最新 run 含图 | 行为 |
|---|---|---|
| false（默认） | 是 | 整请求切 vision 上游（现有行为） |
| false | 否 | main（历史图脱敏） |
| true | 是 | vision 解析图 → 替换 → main |
| true | 否 | main（历史图脱敏，同 false） |

## 5. 代码改动清单

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | `Config.VisionPreprocess bool`（yaml: `vision_preprocess`） |
| `internal/route/route.go` 或 `internal/convert/` | `ExtractLatestImages(format, body) (images []ImageData, userText string, remainingBody []byte, err)`——提取 run 内图（含 tool_result 内嵌）+ 用户文本，图块替换为占位标记 `{"type":"text","text":"[image-pending]"}`；`FillImageText(format, body, []解析文本)` 或直接返回替换后 body |
| `internal/server/server.go` / `convert.go` | `preprocessVision` 编排：构造 vision 请求 → `upstream.Client.Do` → 解析 → 替换；挂接在 `Decide` 后 |
| `internal/route/route.go` | `Decide`：`VisionPreprocess` 时最新 run 含图 → `Upstream: "main"` + `NeedPreprocess` 标记 |
| `README.md` / `prism-proxy.yaml.example` | 配置说明 |

## 6. 测试计划

- 单测：
  - `ExtractLatestImages`：run 内 image 提取（claude image block / tool_result 内嵌）、用户文本提取（排除 tool_result）、无图 no-op、非法 body。
  - 替换：占位 → 解析文本；多图顺序对应。
- 集成（mock vision 上游返回固定解析文本）：
  - `vision_preprocess: true` + 最新 run 含图 → main 收到**无图** body（图块被解析文本替换），mock vision 收到只含图 + prompt 的请求。
  - vision 上游 500 → 客户端 400 显式报错。
  - `vision_preprocess: false` → 现有行为回归（整请求切 vision）。
  - 历史图脱敏与预处理共存（历史图标记 + 最新图解析文本）。
- 验证：`go build ./...` + `go test ./...`（80%+ 覆盖率）。

## 7. 风险

- 延迟 +1 次 vision round-trip（约 2-5s，阻塞式）——接受（正确性优先）。
- 解析质量依赖 vision 模型与 prompt——`vision_preprocess: false` 可随时回退。
- main 模型回复基于解析文本，可能丢失图片细节（如坐标/小字）——prompt 引导详细描述缓解。
