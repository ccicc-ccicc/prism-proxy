# prism-proxy 设计文档

日期：2026-08-06
状态：已批准

## 1. 目标

本地 LLM API 代理服务（单二进制，Go + cobra）：

1. **双协议互通**：同时暴露 OpenAI 格式（`/v1/chat/completions`）与 Claude Messages 格式（`/v1/messages`）入口，内部完成协议转换后转发上游
2. **图片自动路由**：开关开启时，带图请求自动改道到独立配置的多模态上游，客户端无感知
3. **多模态独立配置**：vision 上游的 baseurl / api key / format / model 与主上游完全解耦

## 2. 架构（方案 C：透传快路径 + 交叉转换器 + 原子映射函数库）

```
                        ┌─────────────────────────────────────────────┐
                        │                 prism-proxy                  │
  Claude Code ──/v1/messages──┐                                      │
                              ▼                                      │
  OpenAI SDK ──/v1/chat/completions──► server（认证→格式识别）          │
                              │                                      │
                              ▼                                      │
                        route（图片检测→vision 切换决策）                │
                              │                                      │
                    ┌─────────┴──────────┐                           │
                    ▼                    ▼                           │
              透传快路径            交叉转换器                           │
              (同格式,改 model)    (O2C / C2O, 组合原子映射函数)         │
                    │                    │                           │
                    └─────────┬──────────┘                           │
                              ▼                                      │
                        upstream（转发→流式 SSE→错误透传）──► 上游 API    │
                              │                                      │
                              ▼                                      │
                        log（model/上游/耗时/状态码/vision_switch）      │
                        └─────────────────────────────────────────────┘
```

### 包结构

| 包 | 职责 |
|---|---|
| `cmd/prism-proxy/` | cobra 入口：`serve`、`version` |
| `internal/config/` | YAML 解析、fsnotify 热加载、`atomic.Pointer[Config]` 原子快照 |
| `internal/server/` | 入站 HTTP：两格式路由、认证中间件、SSE 写入 |
| `internal/route/` | 轻量解析入站请求 → 图片检测 → 决策 `{目标上游, 实际模型名, 是否切 vision}` |
| `internal/convert/` | 原子映射函数 + O2C/C2O 转换器 + 流式转换状态机 |
| `internal/upstream/` | 出站客户端：连接复用、超时、错误体透传 |
| `internal/log/` | slog 结构化请求日志 |

### 请求数据流

入站 → 认证（可选）→ 按路径识别格式 → 路由层轻量解析（图片检测，不全量反序列化）→ 决策 → 透传或转换 → 上游 → 响应转换回写 → 请求日志一行。

## 3. 配置模型（YAML，最终定稿）

```yaml
# prism-proxy.yaml
server:
  listen: ":8787"
  auth_keys: []              # 空 = 关闭认证

auto_switch_vision: true     # 全局图片切换开关

main:                        # 主上游：必填
  baseurl: "https://api.openai.com/v1"
  api_key: "sk-xxx"
  format: openai             # openai | claude
  model: "gpt-4o"            # 每个上游单模型
  timeout: 120s              # 上游超时，可选，默认 120s

vision:                      # 可选；多模态上游，与主上游完全解耦
  baseurl: "https://vision.example.com/v1"
  api_key: "sk-vision"
  format: claude
  model: "gpt-4o-vision"
```

- 上游以顶层键声明（YAML map），key 即上游名；无 `upstreams` 列表
- 每个上游单模型（`model` 字段），无模型名映射层：客户端名即上游名
- 上游支持 `timeout`（可选，默认 120s）

### 启动校验（fail fast，panic）

- `main` 缺失或 baseurl / api_key / model 为空 → 启动 panic
- `auto_switch_vision: true` 且 `vision` 未配置或配置不完整 → 启动 panic
- `auto_switch_vision` 缺省值：**false**（保守默认，显式开启才切换）

### 认证（入站）

- `auth_keys` 为空 → 关闭认证（默认）
- 非空 → 每个请求校验，命中任一 key 即通过：
  - OpenAI 路径：`Authorization: Bearer <key>`
  - Claude 路径：`x-api-key: <key>`
  - 两个头都查，任一命中即过
- 校验失败 → 401 + 入站格式错误体

### 热加载

- fsnotify 监听配置文件；编辑器 rename 保存会丢失 inode watch → 收到 `Rename`/`Create` 事件时重新 add watch
- 变更 → 重新解析 → 校验 → `atomic.Pointer` 原子替换（请求间一致性）
- 校验失败 → 保留旧配置，记 `config_reload` 错误日志，不中断服务

## 4. 路由决策（忽略请求模型名）

请求中的模型名**完全忽略**。决策表（顺序判断）：

```
1. 带图 && auto_switch_vision && vision 已配置
   → 上游=vision，模型改写为 vision.model，vision_switch=true，tools 保留
2. 其他（纯文本 / 带图无 vision / 开关关）
   → 上游=main，模型改写为 main.model
```

- 图片检测：route 层**完整反序列化**入站请求为结构化类型（请求体最终必被改写 model，双解析无收益），基于结构检测而非子串扫描：
  - OpenAI：`messages[].content` 数组含 `type=image_url` 块（含 Claude `tool_result` 场景下 user 消息内嵌图片的等价形态）
  - Claude：`content[]` 数组含 `type=image` 块，**递归进入 `tool_result.content`**（工具返回图片的合法场景）
- 带图但 vision 未配置：启动时已 panic（开关开），此处为运行时兜底逻辑
- 切换时 tools 原样保留（兼容 Claude Code 等带工具客户端）
- 响应 `model` 字段回传上游实际模型名

## 5. 协议转换

### 四象限处理路径

| 入站 → 出站 | 处理 |
|---|---|
| OpenAI → OpenAI | 透传：反序列化 → 改 `model` 字段 → 认证头按上游格式替换 → 重发（结构改写，非字节相同） |
| Claude → Claude | 透传：同上 |
| OpenAI → Claude | O2C 转换器（组合原子映射函数） |
| Claude → OpenAI | C2O 转换器（组合原子映射函数） |

出站认证头按上游 format 替换：OpenAI 上游 `Authorization: Bearer <key>`；Claude 上游 `x-api-key: <key>` + `anthropic-version: 2023-06-01`。

### 请求映射（原子函数，双向复用）

| 语义 | OpenAI | Claude |
|---|---|---|
| 系统提示 | `messages[role=system]` | 顶层 `system` 字段（多 system 合并） |
| 用户/助手 | `messages` | `messages` |
| 工具结果 | `role=tool` | user 消息 + `tool_result` block |
| 工具定义 | `tools[].function{name,description,parameters}` | `tools[].input_schema` |
| 工具调用 | `tool_calls[]{id,function{name,arguments(JSON串)}}` | `tool_use` block（`input` 为对象） |
| 图片 | `content[]{type:image_url, url}` | `content[]{type:image, source{type:base64, media_type, data}}` |
| 图片 O2C：`media_type` 来源 | data URL 前缀（`data:image/png;base64,…`）| 内联图：解析前缀；外链图：HTTP 响应 `Content-Type`；两者都缺 → 400 拒绝 |
| 图片 C2O | 构建 data URL（`data:{media_type};base64,{data}`）| `content[]{type:image, source{type:base64, media_type, data}}` → 直接复用 block 内字段 |
| 图片外链（O2C） | 同上 | 代理下载 → base64（超时 30s）；下载失败 → 报错透传 |
| thinking 块 | — | 出站 OpenAI 时剥离；出站 Claude 时透传 |

### 流式映射（SSE 状态机）

Claude 事件全序列：`message_start → content_block_start → content_block_delta* → content_block_stop → [下一 block]… → message_delta → message_stop`，另有 `ping`（保活 `data: {}`）与 `stats` 事件。

| OpenAI 事件 | Claude 事件 |
|---|---|
| `data: {delta:{role}}`（首块） | `message_start`（构造 `message{id, type, role:assistant, content:[], model, stop_reason:null, stop_sequence:null, usage}`；id 按 §6 生成） |
| `data: {delta:{content}}` | `content_block_start(text)` + `content_block_delta(text_delta)` + `content_block_stop` |
| `data: {delta:{tool_calls[]{id,name}}}` | `content_block_start(tool_use)` + `input_json_delta`（增量原样转发）+ `content_block_stop` |
| `data: {delta:{finish_reason}}` | `message_delta{stop_reason}` |
| `data: {usage}` | `message_delta{usage}` |
| `data: [DONE]` | `message_stop` |

反向（C2O）：
- `message_start` → 首个 `data: {choices:[{index:0, delta:{role:"assistant", content:""}, finish_reason:null}]}`
- `content_block_start(text)`/`text_delta`/`content_block_stop(text)` → `delta.content` 增量
- `content_block_start(tool_use)` → `delta.tool_calls[{index, id, type:"function", function:{name, arguments:""}}]`
- `input_json_delta` → `delta.tool_calls[{index, function:{arguments:增量}}]`
- `message_delta{stop_reason}` → `delta.finish_reason`
- `message_delta{usage}` → 末块 `usage` 字段（客户端忽略即可）
- `message_stop` → `data: [DONE]`
- **`ping`/`stats` 吞掉，不转发**（严格 OpenAI SDK 解析器不认识）

Claude 上游请求补 `anthropic-version: 2023-06-01` 头。

### 响应与错误

- 非流式响应同样经原子映射函数转换（O2C：`message.content` 字符串 → text block、`tool_calls` → tool_use blocks、`arguments` JSON 串 parse 为对象；C2O：blocks → content 字符串拼接 + `tool_use` → `tool_calls`、`input` 对象序列化为 JSON 串）
- `model` / `id` / `usage` 双向映射：prompt_tokens→input_tokens、completion_tokens→output_tokens
- 非 2xx：**状态码 + 错误体原样透传**。注意跨象限时客户端拿到的是**上游格式**的错误体（非客户端自身格式）——已知取舍，SDK 解析失败时降级展示原文
- 请求体/响应体上限：入站请求 50MB（含 data URL 图）、非流式响应 50MB；超限 413
- 超时/重试：上游 `timeout` 默认 120s，语义为**连接 + 响应头超时**（流式总时长不限，另有流空闲读超时 60s），不自动重试；连接复用（`http.Transport` 默认池）

## 6. 协议差异坑与处理（已评估）

### 请求方向 O2C

| 坑 | 处理 |
|---|---|
| `max_tokens` OpenAI 可选、Claude 必填 | 缺省补默认值（4096）；`max_completion_tokens` 映射过来 |
| OpenAI `n>1`、`logit_bias`、`seed`、`stream_options`、`response_format` Claude 无 | 剥离；`n>1` 拒绝（400 + 入站格式错误体） |
| `tool_calls[].arguments` JSON 串 → Claude `input` 对象 | `json.Unmarshal`；非法 JSON → `{}` + 日志警告 |
| system 消息可在任意位置 | 合并到顶层 `system`；夹在中间记录警告（罕见） |
| tool 消息 → user + `tool_result`，按 id 配对 | 无配对 → 透传上游错误 |
| 图片外链 Claude 只收 base64 | 代理下载转 base64（带超时）；失败 → 报错透传 |

### 请求方向 C2O

| 坑 | 处理 |
|---|---|
| `top_k`、`thinking`、`cache_control` OpenAI 无 | 剥离（thinking 请求参数剥掉） |
| `tool_result` 的 `is_error`、content 数组 | OpenAI tool 消息无此结构 → 错误文本拼进 content；数组转文本 |
| tool_result 属于 user 消息 | 拆成独立 `role=tool` 消息；**emit 顺序：tool 消息在前、user 文本在后**（OpenAI 要求 role=tool 紧跟 assistant(tool_calls) 消息，否则 400） |
| 历史中 tool_result 无配对 tool_use | 透传上游错误 |
| `max_tokens` 必填 | 映射到 OpenAI `max_tokens`（兼容面最广） |
| 顶层 `system` 多段 | `\n` 连接为一条，放 `messages[0]`（role=system） |

### 响应方向（双向）

| 坑 | 处理 |
|---|---|
| id 格式：`msg_*` vs `chatcmpl-*` | 转换时生成目标格式 id，不原样透传 |
| 流式 block index 规则**按方向区分** | C2O：Claude block index 含 text 占位，**只对 tool_use 独立计数**（0,1,2…）映射 OpenAI `tool_calls[].index`；O2C：Claude `content_block` index 是**全局共享**（text 0、tool_use 1…），由转换器按序生成，不做独立计数 |
| OpenAI 流式 finish_reason 与最后内容同块；Claude 要求独立 `message_delta` 事件 | 状态机拆分：内容块结束 → 再发 stop_reason 事件 |
| Claude usage 在 `message_delta`，OpenAI 流式默认无 usage | 放最后一块的 `usage` 字段（客户端忽略即可） |
| thinking 流式块（含 `signature`/`redacted_thinking`） | 全部剥离，不产生任何 OpenAI 事件；出站 Claude 时透传 |
| `stop_reason` ↔ `finish_reason` | end_turn↔stop、tool_use↔tool_calls、max_tokens↔length；无对应项兜底：stop_sequence→stop、refused/paused/interrupted/limit_exceeded→stop、content_filter→stop（保证不输出枚举外值） |
| 流中途上游错误/断连 | 断流 + 记日志，不伪造结束事件 |
| 大响应体 | 非流式 body 限 50MB（入站同限）；未知字段丢弃、必需字段重建 |
| 外链图片下载（SSRF 提示） | 仅允许 http/https scheme；本地工具场景不做私网段拦截，文档注明风险 |

## 7. 日志

请求日志（slog JSON，每请求一行，两入口同格式）：

```json
{
  "ts": "2026-08-06T18:00:00.000+08:00",
  "level": "INFO",
  "msg": "proxy_request",
  "inbound": "openai",
  "upstream": "main",
  "outbound": "openai",
  "model": "gpt-4o",
  "vision_switch": false,
  "stream": true,
  "status": 200,
  "duration_ms": 1240,
  "error": ""
}
```

- 配置加载/热加载变更单独 `config_reload` 日志
- 图片检测命中但不切换（开关关）记录 `image_detected: true`

## 8. 测试策略

1. **单元**：每个原子映射函数表驱动测试——消息（system 合并、tool 配对、图片 data URL/base64/外链、media_type 缺失拒绝）、工具（JSON 串↔对象、非法 JSON 兜底）、流式状态机（事件序列拆分、C2O 独立 block 计数、O2C 全局 block 计数、thinking 剥离、ping/stats 吞掉、usage/finish_reason 映射）、id 生成格式
2. **集成**：mock 上游（`httptest.Server`）——OpenAI 格式 + Claude 格式各一个，四象限 × {非流式、流式、工具调用、带图} 全矩阵；**vision 切换路径用 Claude 格式 vision 上游**（对齐示例配置），覆盖 media_type 构造与 block 计数
3. **配置**：启动 panic 校验（main 缺失、开关开但 vision 缺失）、热加载原子替换、校验失败保留旧配置、fsnotify rename 后 watch 恢复
4. **路由决策**：模型名被忽略（任意模型名 → 上游始终收到 main.model/vision.model）、开关矩阵（开/关 × 带图/纯文本 × vision 有/无）、`tool_result.content` 内嵌图片检测
5. **认证**：auth_keys 命中/拒绝/默认关闭，OpenAI 头与 Claude 头路径
6. **真实链路**：Claude Code `ANTHROPIC_BASE_URL=http://127.0.0.1:8787` + OpenAI SDK 各发一次带图/纯文本请求
7. **回归**：错误透传（401/429/500 原样）、SSE 断连、外链图片下载失败

## 9. 非目标

- 无 UI、无多用户/计费/配额
- 无负载均衡/故障转移
- 无模型名匹配路由（请求模型名忽略，按 §4 决策表）
- 无模型名映射层（客户端名即上游名；未来需要可加回 `upstream_model` 字段，路由层不依赖）
- 图片切换只做"请求级整段改道"，不做"失败后重试切换"
