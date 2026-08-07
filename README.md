# prism-proxy

本地 LLM API 代理：OpenAI <-> Claude 双协议网关。一个端口同时暴露 OpenAI `chat/completions` 与 Claude `messages` 两套协议，按路由规则转发到配置的上游，并自动完成请求/响应/流式的跨格式转换。

## 功能概述

- **双协议网关**：同一监听端口同时接受 OpenAI 格式（`POST /v1/chat/completions`）与 Claude 格式（`POST /v1/messages`）请求；上游配置为 `openai` 或 `claude` 均可，四象限（同格式透传、O2C、C2O）自动转换，含流式（SSE）与工具调用（tool_calls / tool_use / tool_result）的完整映射。
- **图片自动切换**：入站请求带图（OpenAI `image_url` / Claude `image`，含 `tool_result` 内嵌）且 `auto_switch_vision: true` 时，自动转发到独立的 `vision` 上游（通常是 Claude），文本请求零变化地走 `main`。
- **独立 vision 上游**：`main` 与 `vision` 可配不同格式/模型/密钥，互不影响。
- **模型改写**：转发时强制改写为配置的模型名，忽略入站请求里的 `model` 字段（`vision_switch` 后走 vision 配置的模型）。
- **配置热加载**：修改 `prism-proxy.yaml` 无需重启，自动生效（fsnotify，支持编辑器原子写）。
- **认证（可选）**：`auth_keys` 为空关闭认证；配置后 `Authorization: Bearer` 与 `x-api-key` 两个头都查，任一命中即过（与入口路径无关）。
- **结构化日志**：每请求一行 slog JSON，含耗时与错误信息（见下方日志字段表）。
- **上游错误原样透传**：上游非 2xx（429/500 等）状态码与错误体不转换、原样回写客户端。
- **默认配置路径**：`serve` 缺省读取 `~/.prism-proxy/settings.yaml`，不存在则启动失败（提示旧默认迁移）；可用 `--config` 显式指定。
- **内容日志（traffic log）**：`logging.enabled: true` 时，每请求一条 JSONL 记录客户端请求、出站请求、上游响应、出站响应四段完整内容（`api_key`/`key` 字段脱敏），写入 `~/.prism-proxy/logs/traffic.log`，100MB 轮转保留 `max_files` 个旧文件。

## 安装构建

需要 Go 1.25.5+（go.mod 声明的版本）。

```bash
go build -o prism-proxy ./cmd/prism-proxy
```

也可直接运行：`go run ./cmd/prism-proxy serve --config prism-proxy.yaml`。

命令行：

```bash
prism-proxy version              # 打印版本
prism-proxy serve                # 启动代理（缺省配置 ~/.prism-proxy/settings.yaml）
prism-proxy serve --config prism-proxy.yaml   # 指定配置
```

## 配置说明

完整字段注释见 [prism-proxy.yaml.example](prism-proxy.yaml.example)，复制为 `prism-proxy.yaml` 后修改。启动时配置校验失败会 **fail-fast** 直接报错退出（例如缺少 `main` 上游、`format` 非法、`auto_switch_vision: true` 但没有 `vision` 上游）。

```yaml
server:
  listen: ":8787"        # 监听地址，默认 ":8787"
  auth_keys: []          # 空 = 关闭认证；如 ["sk-proxy-1"]

auto_switch_vision: true # false = 永不切换

main:                    # 主上游（必填）
  baseurl: "https://api.openai.com/v1"
  api_key: "sk-xxxx"
  format: openai         # openai | claude
  model: "gpt-4o"
  auth: ""               # 出站认证头：空 = 按 format 默认（openai→Bearer，claude→x-api-key）；
                         # 显式 bearer / x-api-key 覆盖。网关类上游（AIGW）claude 形态却要求 Bearer，用 auth: bearer
  timeout: 120s          # 连接+响应头超时（Go duration 字符串），默认 120s

vision:                  # 可选；auto_switch_vision: true 时必填（否则启动失败）
  baseurl: "https://api.anthropic.com/v1"
  api_key: "sk-ant-xxxx"
  format: claude
  model: "claude-3-5-sonnet"
```

`logging` 段（可选，默认关闭）：
- `enabled`：内容日志开关，默认 `false`。`enabled` 支持热切换（运行中改配置自动生效）；`dir`/`max_files` 变更需重启。
- `dir`：日志目录，默认 `~/.prism-proxy/logs`（支持 `~` 前缀展开；若为相对路径按当前工作目录）。
- `max_files`：轮转保留的旧文件数，默认 `3`（`traffic.log`、`traffic.log.1`、...）。
- 轮转阈值固定为 lumberjack 默认 100MB。

## 路由规则

1. **带图请求 + `auto_switch_vision: true` + 已配置 vision 上游** → 转发 `vision` 上游，模型改写为 `vision.model`。
2. **其余所有请求**（文本、开关关闭、无 vision 上游） → 转发 `main` 上游，模型改写为 `main.model`。

请求里的 `model` 字段被忽略（配置优先）。上游超时默认为 120s；流式响应整体时长不限，按块控制空闲超时（60s）。请求体上限 50MB，`n > 1` 的请求返回 400。

## 客户端接入

### Claude Code

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY=sk-test   # auth_keys 为空时任意值；配置认证后填任一 auth_key
claude
```

Claude Code 发出 `POST /v1/messages`，代理按路由规则转发。

### OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="sk-test")
```

或直接 curl：

```bash
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-test" \
  -d '{"model":"ignored","messages":[{"role":"user","content":"hello"}]}'
```

### 认证

- `auth_keys: []`：认证关闭，任意 key 放行。
- 配置了 key：两个头都查——`Authorization: Bearer <key>` 与 `x-api-key: <key>` 任一命中即过（与入口路径无关）；都不匹配返回 401，错误体为入站协议信封（OpenAI 格式 `{"error":{"message":"invalid api key","type":"authentication_error"}}`，Claude 格式 `{"type":"error","error":{...}}`）。

## 安全注意事项

- **外链图片下载（SSRF 风险）**：O2C 方向（OpenAI 客户端 → Claude 上游）会为请求中的外链图片（`image_url` 为 `http://`/`https://`）发起代理下载，再内联为 base64。下载仅允许 http/https 协议、跟随最多 3 次重定向、响应体上限 50MB、超时 30s。**风险**：代理可能访问任意主机，包括内网/云元数据地址（如 `http://169.254.169.254/`、`http://127.0.0.1/` 上的内部服务），图片 URL 由调用方控制。请仅在可信本机环境运行本代理，不要向不可信调用方开放；在共享/多租户网络部署时需自行补充白名单或网络隔离。
- 认证密钥经环境/配置文件管理，勿提交到版本库。

## 热加载

`fsnotify` 监听配置文件：修改 `model`、`baseurl`、`auth_keys` 等任意字段后保存即生效，无需重启。成功重载日志：

```
msg="config reloaded" path=prism-proxy.yaml
```

配置校验失败时保留旧配置并记 `config reload failed, keeping old config`，服务不中断。支持编辑器原子写（rename/create/remove 替换）与 vim 风格 two-phase 保存。

`logging.enabled` 支持热切换（true→false 停止内容日志；false→true 需重启，因 TrafficLog 在启动时构造）。

## 日志字段

每请求一行（slog JSON，`msg="proxy_request"`），两协议入口同格式：

| 字段 | 含义 |
|---|---|
| `inbound` | 入站协议 `openai` / `claude` |
| `upstream` | 路由目标 `main` / `vision` |
| `outbound` | 上游协议 `openai` / `claude` |
| `model` | 改写后的模型名 |
| `vision_switch` | 是否触发图片切换（bool） |
| `image_detected` | 检测到图片但未切换（开关关/无 vision 上游）时为 `true` |
| `stream` | 是否流式（bool） |
| `status` | 回写客户端的 HTTP 状态码 |
| `duration_ms` | 请求耗时（毫秒） |
| `error` | 错误信息（成功时为空） |

示例：

```json
{"ts":"2026-08-06T18:00:00+08:00","level":"INFO","msg":"proxy_request","inbound":"openai","upstream":"main","outbound":"openai","model":"gpt-4o","vision_switch":false,"stream":false,"status":200,"duration_ms":1240}
```

配置加载/热加载变更单独记 `config reloaded` 日志；上游请求失败回 502（`upstream error: ...`）。

## 内容日志（traffic log）

`logging.enabled: true` 时，每个请求写一条 JSONL 到 `<dir>/traffic.log`，字段：

| 字段 | 含义 |
|---|---|
| `ts` | 请求开始时间（RFC3339） |
| `request_id` | 请求 ID，同时写入响应头 `X-Request-Id` 与 slog `proxy_request` 行的 `request_id` 字段，用于关联 |
| `inbound` | 入站协议 `openai` / `claude` |
| `upstream` | 路由目标 `main` / `vision` |
| `outbound` | 上游协议 |
| `model` | 改写后的模型名 |
| `stream` | 是否流式 |
| `status` | 回写客户端的状态码 |
| `duration_ms` | 请求耗时 |
| `inbound_body` | 客户端请求体（脱敏后） |
| `outbound_body` | 发给上游的请求体（脱敏后） |
| `upstream_response` | 上游响应体（流式为完整 SSE 文本，脱敏后） |
| `outbound_response` | 回写客户端的响应体（脱敏后；同格式透传时与 upstream_response 相同） |
| `error` | 错误信息（成功为空） |

脱敏规则：四段内容统一处理，JSON 中 `api_key` / `key` 字段的值替换为 `sk-***`；非 JSON 内容原样记录；认证头（`Authorization`、`x-api-key`）不进入日志。日志写入失败不阻塞请求（仅记录 slog 错误）。流式内容超 32MB 时转写日志目录临时文件（内容完整记录，不截断）。

## 验证方法

真实链路（mock 上游 + curl 四象限 + 带图切换 + 错误透传 + 热加载）的完整步骤与实测输出见 [docs/superpowers/specs/2026-08-06-prism-proxy-verification.md](docs/superpowers/specs/2026-08-06-prism-proxy-verification.md)。

快速冒烟：

```bash
# 1. 构建并启动（需先写好 prism-proxy.yaml）
go build -o prism-proxy ./cmd/prism-proxy
./prism-proxy serve --config prism-proxy.yaml

# 2. OpenAI 格式请求（同格式透传）
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer sk-test" \
  -d '{"model":"ignored","messages":[{"role":"user","content":"hi"}]}'

# 3. Claude 格式请求（C2O 交叉）
curl -s http://127.0.0.1:8787/v1/messages \
  -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

# 4. 带图请求（应切到 vision 上游）
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer sk-test" \
  -d '{"model":"ignored","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}'

# 5. 热加载：修改 prism-proxy.yaml 中 main.model 保存，无需重启
tail -f /tmp/prism-proxy.log   # 应看到 msg="config reloaded"
```

## 测试

```bash
go test ./...        # 单元 + 集成测试
go test -race ./...  # 竞态检测
go test ./... -coverprofile=/tmp/cov.out && go tool cover -func=/tmp/cov.out | tail -1
```

## 非目标（有意不做）

无速率限制、无多租户配额、无鉴权粒度细分（全局 auth_keys）、无日志采样。内容日志由 `logging.enabled` 开关控制，`dir`/`max_files` 不支持热更新（仅 `enabled` 可热切换）。详见设计文档 [docs/superpowers/specs/2026-08-07-traffic-log-design.md](docs/superpowers/specs/2026-08-07-traffic-log-design.md)。
