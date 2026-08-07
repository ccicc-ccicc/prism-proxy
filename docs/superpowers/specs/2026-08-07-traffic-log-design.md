# prism-proxy 新增需求设计文档：默认配置路径 + 内容日志

日期：2026-08-07
状态：已批准

## 1. 目标

1. **默认配置路径**：`prism-proxy serve` 不指定 `--config` 时，自动使用 `~/.prism-proxy/settings.yaml`；该文件不存在则启动失败（fail-fast）并给出明确报错。
2. **内容日志（traffic log）**：支持开启后记录每次请求的完整四段内容——客户端传入请求、发给上游的请求、上游返回的响应、回写客户端的响应；日志写入 `~/.prism-proxy/logs/`，按文件大小轮转，保留指定数量的旧文件。

## 2. 需求 1：默认配置路径

### 现状

- `serve` 命令 `--config` 标志默认值为相对路径 `prism-proxy.yaml`（[main.go](cmd/prism-proxy/main.go) L81）。
- 配置加载失败（含文件不存在）已 fail-fast：`NewWatcher` → `Load` 返回错误，`RunE` 返回 error 退出（main.go L52-55）。

### 改动

- `--config` 默认值改为 `~/.prism-proxy/settings.yaml`。
- 启动时用 `os.UserHomeDir()` 展开 `~` 得到真实路径；展开失败或文件不存在时报错退出（复用现有 fail-fast 路径，错误信息提示期望路径）。
- 显式 `--config <path>` 行为不变：按指定路径加载，失败同样 fail-fast。

## 3. 需求 2：内容日志（traffic log）

### 3.1 配置模型（settings.yaml 新增段）

```yaml
logging:
  enabled: false                 # 默认关闭；true 时启用内容日志
  dir: ~/.prism-proxy/logs       # 日志目录；默认 ~/.prism-proxy/logs
  max_files: 3                   # 轮转保留的旧文件数；0 = 不删除旧文件（lumberjack 默认 0）
```

- 轮转阈值采用 lumberjack 默认 `MaxSize = 100MB`（不提供配置字段，按用户确认移除 `max_size`）。
- `dir` 支持 `~` 前缀，启动时展开；`enabled: true` 时若目录不存在则自动创建（`os.MkdirAll`），创建失败 fail-fast。

### 3.2 新增包 `internal/trafficlog`

| 组件 | 职责 |
|---|---|
| `TrafficLog` | lumberjack `io.Writer` 封装：写入 `<dir>/traffic.log`，JSONL 追加写，按大小轮转 |
| `RequestLog` | 单条日志的结构体：元信息 + 四段内容 + 脱敏函数 |
| `Redact` | body 脱敏：JSON 中 `api_key` / `key` 字段值替换为 `sk-***` |

#### lumberjack 配置

```go
&lumberjack.Logger{
    Filename:   filepath.Join(dir, "traffic.log"),
    MaxSize:    100,      // MB，lumberjack 默认值
    MaxBackups: maxFiles, // 保留旧文件数
    Compress:   false,    // 不压缩，便于排查
}
```

#### 日志条目格式（JSONL，每请求一条）

```json
{
  "ts": "2026-08-07T10:00:00+08:00",
  "request_id": "a1b2c3d4e5f6",
  "inbound": "openai",
  "upstream": "main",
  "outbound": "claude",
  "model": "claude-3-5-sonnet",
  "stream": false,
  "status": 200,
  "duration_ms": 1234,
  "inbound_body": "{\"model\":\"...\"}",
  "outbound_body": "{\"model\":\"...\"}",
  "upstream_response": "{\"id\":\"...\"}",
  "outbound_response": "{\"id\":\"...\"}",
  "error": ""
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `request_id` | 每请求生成的短 ID（时间戳 + 随机 hex），用于关联排查 |
| `inbound_body` | 客户端传入的原始请求体（脱敏后） |
| `outbound_body` | 发给上游的请求体（转换/改写后，脱敏后） |
| `upstream_response` | 上游返回的原始响应体（非流式为完整 body；流式为完整 SSE 文本） |
| `outbound_response` | 回写客户端的响应体（转换后；流式为完整 SSE 文本） |
| `error` | 出错时非空（转换失败 / 502 / 413 等）；成功为空 |

### 3.3 脱敏规则

- body 为 JSON 时，递归查找 `api_key` / `key` 字段，其字符串值替换为 `sk-***`（保留原值长度不影响）。
- 非 JSON body 原样记录。
- 认证头（`Authorization`、`x-api-key`）不进入日志：日志只记录 body 内容，天然不落认证头。

### 3.4 记录时机与旁路注入（仅 `enabled: true` 生效）

- **入站请求**：`ServeHTTP` 读入 body 后记录 `inbound_body`（body 已被 `io.ReadAll` 读取，直接取副本）。
- **出站请求**：`relay` 中转换/改写完成后，记录 `outbound_body`（透传路径为 `rewriteModel` 产物，交叉路径为转换产物）。
- **上游响应（非流式）**：`forward` 中 `io.ReadAll` 读取上游响应后记录。
- **上游响应（流式）**：`copyStream` / `convertStream` 回写处，用 `io.TeeReader` / `io.MultiWriter` 旁路复制到日志累积缓冲，流结束后整体作为 `upstream_response` / `outbound_response` 写入。
- **出站响应（非流式）**：交叉转换后 `json.NewEncoder(w).Encode(out)` 前记录；同格式透传路径上游响应即出站响应（复用同一份）。
- 日志写入失败不阻塞请求处理：仅记 slog 错误。
- `enabled: false` 时完全不注入旁路，零开销。

### 3.5 流式完整记录实现要点

- 流式路径的所有写入点（`copyStream`、`convertStream` 的回写、`Forward` 的透传）都经过 `w.Write`，在这些点用 `io.MultiWriter(w, logBuf)` 旁路：
  - 同格式透传：`io.MultiWriter(w, &upstreamBuf)` 与 `io.MultiWriter(w, &outboundBuf)`（两段内容相同）。
  - 交叉转换：上游原始帧从 `convertStream` 读入处旁路累积到 `upstreamBuf`；回写客户端的转换后帧旁路累积到 `outboundBuf`。
- 流结束后，将累积内容写入日志条目；中途断流/错误也写入已累积部分并填充 `error`。

### 3.6 错误处理

| 场景 | 行为 |
|---|---|
| 日志写入失败（磁盘满等） | 不阻塞请求，slog 记 `traffic log write failed` |
| `dir` 目录创建失败 | fail-fast 退出 |
| 请求转换失败 / 上游 502 / 413 | 记录已获取内容 + `error` 字段 |
| 认证失败（401） | 不记内容日志（未读 body，也无路由信息），保持现状仅 slog |

## 4. 改动文件清单

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | 新增 `LoggingConfig{Enabled, Dir, MaxFiles}`、默认值、`Validate` 扩展 |
| `internal/config/config_test.go` | 新字段解析/默认值/校验测试 |
| `internal/trafficlog/trafficlog.go` | 新包：`TrafficLog`（lumberjack 封装）、`RequestLog`、`Redact` |
| `internal/trafficlog/trafficlog_test.go` | 新包测试：脱敏、JSONL 格式、轮转（小 MaxSize mock） |
| `internal/server/server.go` | `ServeHTTP` 记录 `inbound_body`、注入 `request_id`；`log` 方法签名扩展 |
| `internal/server/convert.go` | `relay`/`forward` 记录 `outbound_body`、`upstream_response`、`outbound_response`；流式旁路 |
| `internal/server/server_test.go` 等 | 四段内容记录、流式完整记录、错误路径测试 |
| `cmd/prism-proxy/main.go` | `--config` 默认值 `~/.prism-proxy/settings.yaml` |
| `prism-proxy.yaml.example` | 新增 `logging` 段示例 |
| `README.md` | 默认配置路径、logging 配置、日志字段表、traffic.log 说明 |
| `go.mod` | 新增 `gopkg.in/natefinch/lumberjack.v2` 依赖 |

## 5. 测试策略

1. **config**：`logging` 段解析（enabled/dir/max_files）、默认值（dir 默认 `~/.prism-proxy/logs`）、`~` 展开、enabled 时目录校验。
2. **trafficlog 单元**：脱敏（`api_key`/`key` 掩码、非 JSON 原样）、JSONL 序列化字段齐全、轮转触发（lumberjack 小 `MaxSize` mock 验证文件数）。
3. **server 集成**：开启日志后四段内容完整记录；流式请求 `upstream_response`/`outbound_response` 为完整 SSE；错误路径（上游 502、转换失败）`error` 字段与已记录内容；关闭日志零旁路。
4. **main**：`--config` 缺省时用 `~/.prism-proxy/settings.yaml`，文件缺失报错退出。

## 6. 非目标

- 不记录认证头（脱敏策略已排除）。
- 不提供行数轮转、按天分文件、压缩、日志采样。
- 不截断超长内容（完整记录，轮转兜底）。
- 不改变现有 slog 元信息日志（`proxy_request` 一行）行为。
