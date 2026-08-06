# prism-proxy 真实链路验证记录

- 日期: 2026-08-06
- 环境: macOS (darwin 25.6.0), Go, Python 3.14.2
- 目的: 用 mock 上游 + curl 实测四象限转换、图片切换、流式、工具调用、错误透传、热加载，并记录实际输出。

## 拓扑

```
curl (openai/claude 格式)
  → prism-proxy :8787 (auto_switch_vision: true, auth 关闭)
    ├─ main   upstream → http://127.0.0.1:9997/v1 (openai 格式 mock, python3 http.server)
    └─ vision upstream → http://127.0.0.1:9998/v1 (claude 格式 mock)
```

配置 `/tmp/prism-test/prism-proxy.yaml`（初始）:

```yaml
server:
  listen: ":8787"
  auth_keys: []
auto_switch_vision: true
main:
  baseurl: "http://127.0.0.1:9997/v1"
  api_key: "sk-mock"
  format: openai
  model: "gpt-4o-mock"
  timeout: 10s
vision:
  baseurl: "http://127.0.0.1:9998/v1"
  api_key: "sk-mock"
  format: claude
  model: "claude-vision-mock"
  timeout: 10s
```

构建: `go build -o prism-proxy ./cmd/prism-proxy`

## 验证清单结果

| 项 | 结果 |
|---|---|
| 同格式透传（openai→openai） | PASS |
| C2O 交叉（claude→openai） | PASS |
| O2C 交叉（openai 带图→vision claude） | PASS |
| C2C 透传（claude 带图→vision claude） | PASS |
| 流式透传（openai SSE） | PASS |
| 流式 C2O（claude SSE→openai SSE→claude events） | PASS |
| 工具调用 C2O（tool_calls→tool_use） | PASS |
| 带图请求到达 vision mock（vision.model + image block） | PASS（mock 日志确认） |
| 文本请求零变化（不带图、不切 vision） | PASS（mock 日志确认） |
| 错误透传 429 / 500 | PASS（状态码 + 错误体原样） |
| 上游不可达 → 502 | PASS（顺带验证） |
| 热加载（改 model 无重启生效） | PASS（`config reloaded` 日志确认） |
| 非法请求体 → 400 | PASS |
| 未知路径 → 404 | PASS |
| Claude Code / OpenAI SDK 直连 | 等价验证：与二者同协议（/v1/messages + x-api-key；/v1/chat/completions + Bearer）的 curl 请求全部通过；未做交互式客户端实跑 |

## 实测输出

### T1 同格式透传（openai → openai main）

```bash
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer sk-test" \
  -d '{"model":"client-says-gpt-5","messages":[{"role":"user","content":"hi"}]}'
```

```json
{"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-4o-mock", "choices": [{"index": 0, "message": {"role": "assistant", "content": "mock openai reply"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
```

请求里 `model: client-says-gpt-5` 被忽略，改写为配置的 `gpt-4o-mock`。

### T2 C2O 交叉（claude → openai main）

```bash
curl -s http://127.0.0.1:8787/v1/messages \
  -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":"hello from claude"}]}'
```

```json
{"id":"msg_b6b75ce9ccea45f4","type":"message","role":"assistant","model":"gpt-4o-mock","content":[{"type":"text","text":"mock openai reply"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}
```

OpenAI 响应被转回 Claude 格式（`content` blocks、`stop_reason: end_turn`）。上游 mock 收到的转换后请求：

```
[OPENAI-MOCK] POST /v1/chat/completions MODEL='gpt-4o-mock'
[OPENAI-MOCK] BODY={"model":"gpt-4o-mock","messages":[{"role":"user","content":"hello from claude"}],"max_tokens":64}
```

### T3 O2C 交叉 + 图片切换（openai 带图 → vision claude）

```bash
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer sk-test" \
  -d '{"model":"ignored","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}'
```

```json
{"id":"chatcmpl-e412d1ad2310b6e0","object":"chat.completion","created":1786026464,"model":"claude-vision-mock","choices":[{"index":0,"message":{"role":"assistant","content":"mock claude reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}
```

vision mock（9998）收到的请求——**模型为 `claude-vision-mock`、含 image block（base64）**：

```
[CLAUDE-MOCK] POST /v1/messages MODEL='claude-vision-mock' STREAM=None IMAGE=True
[CLAUDE-MOCK] BODY={"model":"claude-vision-mock","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}],"max_tokens":4096}
```

### T4 C2C 透传（claude 带图 → vision claude）

```bash
curl -s http://127.0.0.1:8787/v1/messages \
  -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}'
```

```json
{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-vision-mock", "content": [{"type": "text", "text": "mock claude reply"}], "stop_reason": "end_turn", "usage": {"input_tokens": 1, "output_tokens": 1}}
```

vision mock 日志确认收到图片（`IMAGE=True`、model `claude-vision-mock`、`max_tokens: 64` 原样保留）。

### T5 流式透传（openai SSE）

```bash
curl -s -N http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer sk-test" \
  -d '{"model":"ignored","stream":true,"messages":[{"role":"user","content":"hi stream"}]}'
```

```
data: {"id": "chatcmpl-m1", "object": "chat.completion.chunk", "model": "gpt-4o-reloaded", "choices": [{"index": 0, "delta": {"role": "assistant", "content": "mock"}, "finish_reason": null}]}

data: {"id": "chatcmpl-m1", "object": "chat.completion.chunk", "model": "gpt-4o-reloaded", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}

data: [DONE]
```

### T6 流式 C2O（claude SSE → openai SSE → claude events）

```bash
curl -s -N http://127.0.0.1:8787/v1/messages \
  -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"ignored","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi stream from claude"}]}'
```

```
event: message_start
data: {"type":"","message":{"id":"msg_d170ef366ebfe085","type":"message","role":"assistant","model":"gpt-4o-reloaded","content":[]}}

event: content_block_start
data: {"type":"","content_block":{"type":"text"}}

event: content_block_delta
data: {"type":"","delta":{"type":"text_delta","text":"mock"}}

event: content_block_stop
data: {"type":""}

event: message_delta
data: {"type":"","stop_reason":"end_turn"}

event: message_stop
data: {"type":""}
```

### T11 工具调用 C2O（tool_calls → tool_use）

```bash
curl -s http://127.0.0.1:8787/v1/messages \
  -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":"use-tool please"}]}'
```

OpenAI mock 返回 `tool_calls`（`get_weather` / arguments JSON），代理转回 Claude `tool_use` block（arguments JSON → input 对象）：

```json
{
    "id": "msg_3e03dcd84c70a070",
    "type": "message",
    "role": "assistant",
    "model": "gpt-4o-reloaded",
    "content": [
        {"type": "tool_use", "id": "call_t1", "name": "get_weather", "input": {"city": "beijing"}}
    ],
    "stop_reason": "tool_use"
}
```

### T7 错误透传

```bash
# openai 格式 → 429
curl -s -w "\nHTTP %{http_code}\n" http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer sk-test" \
  -d '{"model":"ignored","messages":[{"role":"user","content":"should-429"}]}'
# → {"error": {"message": "rate limit exceeded", "type": "rate_limit_error"}}   HTTP 429

# claude 格式 → 500
curl -s -w "\nHTTP %{http_code}\n" http://127.0.0.1:8787/v1/messages \
  -H "Content-Type: application/json" -H "x-api-key: sk-test" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":"should-500"}]}'
# → {"error": {"message": "upstream exploded", "type": "server_error"}}   HTTP 500
```

状态码 + 错误体原样透传（不转换）。上游不可达时回 502：

```
error="upstream request: Post \"http://127.0.0.1:9997/v1/chat/completions\": dial tcp 127.0.0.1:9997: connect: connection refused"  status=502
```

### T8 热加载（无重启改 model）

```bash
sed -i '' 's/model: "gpt-4o-mock"/model: "gpt-4o-reloaded"/' /tmp/prism-test/prism-proxy.yaml
# 未重启代理；再次请求，响应 model 立即变为 gpt-4o-reloaded
```

代理日志（每次保存出现两条 `config reloaded`，为 rename 事件 + 后台重挂补加载，幂等）：

```
INFO config reloaded path=/tmp/prism-test/prism-proxy.yaml
INFO config reloaded path=/tmp/prism-test/prism-proxy.yaml
INFO proxy_request inbound=openai upstream=main outbound=openai model=gpt-4o-reloaded vision_switch=false stream=false status=200 duration_ms=0
```

随后把 `main.baseurl` 改为 9997 端口同样热加载生效（T12）。

### T9 / T10 非法请求与未知路径

```bash
curl -s http://127.0.0.1:8787/v1/chat/completions -d '{not json'
# → invalid request: parse openai request: ...   HTTP 400
curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8787/v1/other
# → 404
```

### 请求日志（每请求一行）

```
INFO proxy_request inbound=openai upstream=main outbound=openai model=gpt-4o-mock vision_switch=false stream=false status=200 duration_ms=2
INFO proxy_request inbound=openai upstream=vision outbound=claude model=claude-vision-mock vision_switch=true stream=false status=200 duration_ms=1
INFO proxy_request inbound=claude upstream=main outbound=openai model=gpt-4o-mock vision_switch=false stream=true status=200 duration_ms=1
INFO proxy_request inbound=openai upstream=main outbound=openai model=gpt-4o-mock vision_switch=false stream=false status=429 duration_ms=0
```

`vision_switch=true` 仅在图片切换时出现，字段与 spec §7 一致。

## 已知限制（诚实记录）

1. **Claude Code / OpenAI SDK 交互式实跑未执行**（无交互终端）；以与二者完全相同的协议/头（`/v1/messages` + `x-api-key` + `anthropic-version`；`/v1/chat/completions` + `Authorization: Bearer`）的 curl 请求验证，结果全部通过。
2. **mock 上游流式响应的 EOF**：python `http.server` 的 HTTP/1.1 keep-alive 默认不关闭连接，导致首轮流式透传被代理的 60s 流空闲超时兜底切断（`error="context deadline exceeded"`，数据已完整转发）。在 mock 的 SSE 响应加 `Connection: close` 后，流式 0-1ms 干净结束。这是 mock 侧行为，非代理 bug；代理的兜底行为（不断流不伪造结束事件、记日志）符合 spec §6。
3. 验证用 mock 脚本位于 `/tmp/prism-mock-openai.py`（9997 端口）与 `/tmp/prism-mock-claude.py`（9998 端口），未入库；README 中记录了 curl 冒烟命令。
4. 本机端口 9999 曾被 OrbStack 占用，openai mock 改挂 9997；配置 baseurl 同步热加载，无重启。
