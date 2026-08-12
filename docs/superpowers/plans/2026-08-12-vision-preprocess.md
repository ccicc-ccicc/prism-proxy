# Vision 预处理实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `vision_preprocess: true` 时，最新 run 的图片经 vision 上游单独解析为文本后替换进请求，main 只收纯文本——结构性消除 vision 上游上下文超限（gpt-5-nano + 486KB 截图实测 400）。

**Architecture:** 三阶段：① 提取最新 run 图片 + 用户文本（convert 纯函数）② vision 上游单独请求解析（server 编排，阻塞式，失败显式 400）③ 解析文本替换图块 → 历史图脱敏 → main。`vision_preprocess: false` 完全保持现有行为（可回退）。

**Tech Stack:** Go 1.25.5+，标准库 `encoding/json`。

## Global Constraints

- `vision_preprocess` 默认 false——false 时行为与现状完全一致（整请求切 vision），生产可随时回退。
- main 上游是纯文本模型（deepseek）；vision 上游窗口小（gpt-5-nano）。
- vision 预处理请求**只含图 + prompt**（绝不带历史）；多图合并一次请求。
- 预处理失败 → 显式 400（不静默丢图）；单图解码失败 → 该图 `[image omitted]` 其余继续。
- map 基遍历（保留未知字段）；无状态（无解析缓存）。
- 用户文本提取排除 tool_result 块。
- 验证：`go build ./...` + `go test ./...`（80%+）。

---

### Task 1: 配置 + 检测调整 + 提取/替换纯函数

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/route/route.go`
- Create/Modify: `internal/convert/images.go`（新文件，提取/替换纯函数）
- Test: `internal/route/route_test.go`、`internal/convert/images_test.go`（新）

**Interfaces:**
- Produces:
  - `config.Config.VisionPreprocess bool`（yaml: `vision_preprocess`，默认 false）
  - `route.Decision.NeedPreprocess bool`（vision_preprocess=true 且最新 run 含图且无 vision 上游缺配时）
  - `convert.ExtractLatestImages(format string, body []byte) (imgs []ImageData, userText string, out []byte, err error)`
    - `ImageData{MediaType, Data string}`（base64）
    - 提取最新 run 内 image 块（含 tool_result 内嵌），图块原位替换为 `{"type":"text","text":"[image-pending]"}`（占位，顺序对应 imgs）
    - userText：run 内 user 消息 text 拼接（排除 tool_result 块、排除图块本身）
    - 无图 → (nil, "", 原 body 字节, nil)
  - `convert.FillImageTexts(format string, body []byte, texts []string) ([]byte, error)`
    - 把占位 `[image-pending]` 依次替换为 `[图片内容: <text>]`；texts 长度不足时余下占位替换为 `[image omitted]`

- [ ] **Step 1: 写失败测试**（`internal/convert/images_test.go` 新建 + `route_test.go` 追加）
  - ExtractLatestImages：claude image block 提取；tool_result 内嵌提取；userText 排除 tool_result；多图顺序；无图 no-op（字节一致）；非法 body error
  - FillImageTexts：占位替换；texts 不足 → omitted；无占位 no-op
  - route_test：`Decide` 在 VisionPreprocess=true + 最新 run 含图 → `NeedPreprocess=true, Upstream="main"`；VisionPreprocess=false → 现有行为（切 vision）；VisionPreprocess=true 无 vision 上游 → 走 main + NeedPreprocess=false
- [ ] **Step 2: 验证失败**：`go test ./internal/convert/ ./internal/route/ -run 'TestExtract|TestFill|TestDecide' -v`
- [ ] **Step 3: 实现**
  - config.go：`VisionPreprocess bool \`yaml:"vision_preprocess"\``
  - route.go `Decide`：`if hasImage && cfg.AutoSwitchVision { if cfg.VisionPreprocess { return Decision{Upstream:"main", Model:cfg.Main().Model, VisionSwitch:false, HasImage:true, NeedPreprocess:true} } ... 现有切 vision 逻辑 }`
  - convert/images.go：map 基实现 ExtractLatestImages / FillImageTexts（run 边界复用 route 的语义：尾部跳过 + assistant(tool_use) 属 run——与 SanitizeHistoryImages 一致，把工具循环算进 run）
- [ ] **Step 4: 验证通过**：目标测试全绿 + `go test ./...`
- [ ] **Step 5: 提交**：`feat: add vision preprocess extraction and config switch`

---

### Task 2: server 编排 + 集成测试 + 文档

**Files:**
- Modify: `internal/server/convert.go`（`relay` 或 ServeHTTP 挂接）
- Modify: `internal/server/server.go`（ServeHTTP 挂接点 + 日志）
- Test: `internal/server/integration_test.go`
- Modify: `README.md`、`prism-proxy.yaml.example`

**Interfaces:**
- Consumes: `convert.ExtractLatestImages`、`convert.FillImageTexts`、`route.Decision.NeedPreprocess`
- Produces: `s.preprocessVision(ctx, cfg, body) ([]byte, error)`——构造 vision 请求（vision 上游格式 openai：image_url part + text prompt；claude：image block + text 块）→ `s.client.Do` → 解析响应文本 → `FillImageTexts`；失败返回 error（由 ServeHTTP 转 400）

- [ ] **Step 1: 写失败测试**（integration_test.go）
  - `TestVisionPreprocess_ImageToMain`：`VisionPreprocess: true`、vision mock 返回 `{"choices":[{"message":{"content":"图中是登录页"}}]}`；main mock 断言收到**无 image_url/无 image 块**、含 `[图片内容: 图中是登录页]`；vision mock 断言收到**只含图 + prompt**（无历史文本）
  - `TestVisionPreprocess_UpstreamFail`：vision mock 500 → 客户端 400
  - `TestVisionPreprocess_Disabled`：`false` → 现有切 vision 行为（vision 收到整请求）
  - 历史图脱敏共存：历史含图 + 最新含图 → main 收到历史标记 + 最新解析文本
- [ ] **Step 2: 验证失败**
- [ ] **Step 3: 实现**
  - server：`ServeHTTP` 在 `Decide` 后：`if decision.NeedPreprocess { body, err = s.preprocessVision(r.Context(), cfg, body); if err != nil → 400 }`；随后现有 main 脱敏（历史图）逻辑照常
  - preprocessVision：ExtractLatestImages → 构造 vision 请求（按 vision.Format 分支）→ client.Do（非流式）→ 解析 OpenAI/Claude 响应文本 → FillImageTexts
  - 日志：`vision_preprocess: true` + `vision_preprocess_ms`
- [ ] **Step 4: 验证通过**：`go test ./...` 全绿
- [ ] **Step 5: 提交**：`feat: wire vision preprocess pipeline with error handling`

---

### Task 3: 构建部署 + 生产验证

- [ ] **Step 1**: `go test ./...` 全量确认 → 构建 arm64 本地镜像 + 多架构推送 harbor（tag = HEAD hash）
- [ ] **Step 2**: ssh 10.244.138.168 拉新镜像、改配置（`vision_preprocess: true`）、重启容器
- [ ] **Step 3**: 生产验证——模拟含图请求（带历史 + 486KB 级大图）经代理 → 期望 200（vision 只收图、main 收解析文本）；验证 traffic log：`upstream=main`、`vision_preprocess` 标记
- [ ] **Step 4**: 提交验证结论到报告

---

## Self-Review

**Spec 覆盖**：§3.1 配置（T1）✓ §3.2 流程①②③④⑤⑥（T1 提取/T2 编排）✓ §3.3 prompt（T2，含用户上下文）✓ §3.4 错误处理（T1 单图降级/T2 失败 400）✓ §3.5 日志（T2）✓ §4 行为矩阵（T1 Decide/T2 集成测试）✓ §6 测试计划（T1/T2）✓ README/yaml.example（T2）✓

**占位符**：无 TBD；实现细节在任务内给出。

**类型一致性**：`ExtractLatestImages`/`FillImageTexts`/`ImageData`/`NeedPreprocess` 全文签名一致；prompt 常量在 T2 定义（`visionDescribePrompt` 固定指令 + 用户上下文拼接）。
