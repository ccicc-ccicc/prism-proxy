# 历史图片智能替换（History Image Sanitize）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code CLI 同会话重放历史导致每轮触发 vision 切换的问题——检测收窄到"末尾用户侧消息 run"，走 main 时历史图片块替换为短标记，纯文本轮次不再触发 vision、main 纯文本模型不再收到图而报错。

**Architecture:** 检测语义收窄（`route.RequestHasImage` → `LatestUserRunHasImage`，只查末尾连续 user/tool 消息段）+ 历史图脱敏（`route.SanitizeHistoryImages`，map 基遍历保留未知字段，图片块替换为短标记，assistant 解析文本原位保留）+ server 转发管线挂接（走 main 且 auto_switch_vision 时脱敏）。

**Tech Stack:** Go 1.25.5+，标准库 `encoding/json`，`log/slog`。

## Global Constraints

- `main` 上游是纯文本模型——历史带图走 main 会 400 报错，脱敏是硬需求。
- 客户端是 Claude Code CLI：同会话每轮重放完整历史（含图块、tool_result 内嵌截图），直到 auto-compact 压缩。
- **脱敏必须 map 基遍历（`map[string]any` / `[]any`），禁止 typed struct 往返**——typed 往返会丢 `cache_control`（块级）、`metadata`（消息级）等未知字段，破坏 main 上游 prompt caching。
- 无状态实现：不引入会话 ID、内存缓存等跨请求状态。
- 不加新配置项；行为由现有 `auto_switch_vision` 门控。
- `auto_switch_vision: false` → 完全原样转发（任何图不处理）。
- 最新（run 内）图片永不脱敏：要么切 vision，要么原样走 main（显式报错）。
- 验证命令：`go test -race ./...`、`go test -cover ./...`（保持 80%+ 覆盖率）。
- 提交格式：`<type>: <description>`（feat/fix/refactor/test/docs/chore）。

---

### Task 1: `LatestUserRunHasImage` —— 检测语义收窄

**Files:**
- Modify: `internal/route/route.go`（`RequestHasImage` 改为 `LatestUserRunHasImage`，重写实现）
- Test: `internal/route/route_test.go`

**Interfaces:**
- Produces: `func LatestUserRunHasImage(format string, body []byte) (bool, error)` — format ∈ {"openai","claude"}；run 内任一消息含图 → true；run 为空（无 user/tool 消息）→ false；解析失败 → error。
- Consumes: `convert.ContentHasImage(parts any) bool`（`internal/convert/messages.go:413`，[]any 形状同时识别 Claude `"image"` 与 OpenAI `"image_url"`，递归 tool_result）——**直接复用，不新写检测逻辑**。
- 后续任务依赖：Task 3 的 `ServeHTTP` 调用它（通过 `route.Decide` 内部）。

- [ ] **Step 1: 写失败测试**

在 `internal/route/route_test.go` 追加（替换现有 `TestDecide_ImageInToolResult` 中 `RequestHasImage` 调用为 `LatestUserRunHasImage`，并新增用例）：

```go
func TestLatestUserRunHasImage_HistoryImageNotTrigger(t *testing.T) {
	// 历史含图（run 之前），最新 run 纯文本 → false（这是本次语义收窄的核心）
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"这张图显示了一个仪表盘。"},
		{"role":"user","content":"那个数字是多少？"}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("history image must not trigger")
	}
}

func TestLatestUserRunHasImage_RunWithImage(t *testing.T) {
	// 末尾 run 内含图 → true
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":"之前的内容"},
		{"role":"assistant","content":"好的"},
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("run image: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_ConsecutiveUserRun(t *testing.T) {
	// 连续 user 消息 run（首条带图、末条纯文本）→ true：新图不被误判为历史
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"user","content":"看这个"}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("consecutive user run: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_AssistantLast(t *testing.T) {
	// 序列以 assistant 结尾 → run 为其前最后一段连续用户侧消息
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"分析完毕"}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("assistant-last run: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_NoUserMessages(t *testing.T) {
	// 无 user/tool 消息 → false
	body := []byte(`{"model":"x","messages":[{"role":"assistant","content":"hi"}]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("no user messages must be false")
	}
}

func TestLatestUserRunHasImage_OpenAIToolRole(t *testing.T) {
	// OpenAI role=tool 消息含图属于 run 成员（等价 Claude tool_result）
	body := []byte(`{"model":"x","messages":[
		{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"screenshot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	]}`)
	has, err := LatestUserRunHasImage("openai", body)
	if err != nil || !has {
		t.Fatalf("tool role image: %v %v", has, err)
	}
}

func TestLatestUserRunHasImage_InvalidBody(t *testing.T) {
	_, err := LatestUserRunHasImage("openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}
```

同时修改现有 `TestDecide_ImageInToolResult`（`route_test.go:54`）：`RequestHasImage("claude", body)` → `LatestUserRunHasImage("claude", body)`。

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/route/ -run 'TestLatestUserRunHasImage|TestDecide' -v`
Expected: FAIL（`LatestUserRunHasImage undefined`，以及 `TestDecide_ImageInToolResult` 编译错误——`RequestHasImage` 尚未改名）

- [ ] **Step 3: 最小实现**

在 `internal/route/route.go` 中**删除 `RequestHasImage` 整个函数**（含现有 openai/claude 分支），替换为：

```go
// LatestUserRunHasImage 检测末尾"用户侧消息 run"（连续 user/tool 消息段，无 assistant 打断）是否含图。
// claude: run = 末尾连续 role=user 消息；openai: run = 末尾连续 role=user|tool 消息。
// 序列以 assistant 结尾时，run 为其前最后一段连续用户侧消息；run 为空 → false。
// 历史（run 之前）消息中的图片不参与检测——由 SanitizeHistoryImages 处理。
func LatestUserRunHasImage(format string, body []byte) (bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false, fmt.Errorf("parse %s request: %w", format, err)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(m["messages"], &msgs); err != nil {
		return false, fmt.Errorf("parse %s request: %w", format, err)
	}
	userRoles := map[string]bool{"user": true}
	if format == "openai" {
		userRoles["tool"] = true
	}
	// 序列以 assistant 结尾时，run 为其前最后一段连续用户侧消息：先跳过尾部非用户侧消息
	end := len(msgs) - 1
	for ; end >= 0; end-- {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgs[end], &msg); err != nil {
			return false, fmt.Errorf("parse %s request: %w", format, err)
		}
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if userRoles[role] {
			break
		}
	}
	for i := end; i >= 0; i-- {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgs[i], &msg); err != nil {
			return false, fmt.Errorf("parse %s request: %w", format, err)
		}
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if !userRoles[role] {
			break // 中间的 assistant 打断 run
		}
		content := msg["content"]
		if len(content) > 0 {
			var parts []any
			if err := json.Unmarshal(content, &parts); err == nil && convert.ContentHasImage(parts) {
				return true, nil
			}
		}
	}
	return false, nil
}
```

`route.go` 现有 import 已含 `encoding/json`、`fmt`、`convert`——无需加 import。

- [ ] **Step 4: 运行验证通过**

Run: `go test ./internal/route/ -run 'TestLatestUserRunHasImage|TestDecide' -v`
Expected: PASS（`TestDecide_*` 现有用例 body 均为单消息，run == 全量，语义不变）

- [ ] **Step 5: 提交**

```bash
git add internal/route/route.go internal/route/route_test.go
git commit -m "refactor: narrow image detection to latest user-run (LatestUserRunHasImage)"
```

---

### Task 2: `SanitizeHistoryImages` —— 历史图片脱敏（map 基）

**Files:**
- Modify: `internal/route/route.go`（新增 `SanitizeHistoryImages` + 私有辅助函数 + 常量）
- Test: `internal/route/route_test.go`

**Interfaces:**
- Produces:
  - `func SanitizeHistoryImages(format string, body []byte) ([]byte, bool, error)` — 返回替换后 body、是否发生替换（bool）、解析错误。
  - 标记常量：`analyzedMark = "[image: analyzed in previous reply]"`、`omittedMark = "[image omitted]"`。
- Consumes: `LatestUserRunHasImage` 的 run 边界语义（§3.1 锚点，本函数内重复计算 run 起点）。

- [ ] **Step 1: 写失败测试**

在 `internal/route/route_test.go` 追加：

```go
const markAnalyzed = "[image: analyzed in previous reply]"
const markOmitted = "[image omitted]"

func TestSanitize_ClaudeImageBlockReplaced(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
		{"role":"assistant","content":[{"type":"text","text":"图中是仪表盘。"}]},
		{"role":"user","content":"数字是多少"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected sanitize")
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)
	first := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != markAnalyzed {
		t.Fatalf("marker: %v", first)
	}
	// assistant 解析文本原位保留
	text := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if text["text"] != "图中是仪表盘。" {
		t.Fatalf("assistant text mutated: %v", text)
	}
	// run 内消息（最后一条）不动
	last := msgs[2].(map[string]any)
	if last["content"] != "数字是多少" {
		t.Fatalf("latest message mutated: %v", last)
	}
}

func TestSanitize_ClaudeNoAssistantText(t *testing.T) {
	// 含图消息后无含文本 assistant 消息 → omitted 标记。
	// 注意：含图消息必须被 assistant 打断（run 边界外），否则两条连续 user 会整体算入 run。
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search","input":{}}]},
		{"role":"user","content":"继续"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	if !strings.Contains(string(out), markOmitted) {
		t.Fatalf("expected omitted marker, got: %s", out)
	}
}

func TestSanitize_ToolLoopFallback(t *testing.T) {
	// 工具循环：截图后紧邻 assistant 是 tool_use（无文本），解析文本在更靠后的 assistant → analyzed
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"search","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"nothing found"}]},
		{"role":"assistant","content":[{"type":"text","text":"截图中是登录页。"}]},
		{"role":"user","content":"继续"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	if !strings.Contains(string(out), markAnalyzed) {
		t.Fatalf("expected analyzed marker (scan across tool loop), got: %s", out)
	}
}

func TestSanitize_OpenAIImageURLReplaced(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"text","text":"看"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"图上有一个按钮"},
		{"role":"user","content":"点哪里"}
	]}`)
	out, changed, err := SanitizeHistoryImages("openai", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	content := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "text" {
		t.Fatalf("parts: %v", content)
	}
	if content[0].(map[string]any)["text"] != "看" {
		t.Fatalf("text part mutated: %v", content[0])
	}
}

func TestSanitize_CacheControlPreserved(t *testing.T) {
	// map 基往返：块级 cache_control 与 tool_result 的 is_error/tool_use_id 保留
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"},"cache_control":{"type":"ephemeral"}}]},
		{"role":"assistant","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":"继续"}
	]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || !changed {
		t.Fatalf("sanitize: %v %v", changed, err)
	}
	if !strings.Contains(string(out), `"cache_control"`) {
		t.Fatalf("cache_control lost: %s", out)
	}
}

func TestSanitize_NoImageNoop(t *testing.T) {
	// 无图 → no-op：body 逐字节一致，sanitized=false
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no change")
	}
	if string(out) != string(body) {
		t.Fatalf("body not byte-identical: %s", out)
	}
}

func TestSanitize_NoUserRunNoop(t *testing.T) {
	// run 为空（无 user/tool 消息）→ no-op
	body := []byte(`{"model":"x","messages":[{"role":"assistant","content":"hi"}]}`)
	out, changed, err := SanitizeHistoryImages("claude", body)
	if err != nil || changed {
		t.Fatalf("expected noop: %v %v", changed, err)
	}
	if string(out) != string(body) {
		t.Fatalf("body not byte-identical: %s", out)
	}
}

func TestSanitize_InvalidBody(t *testing.T) {
	_, _, err := SanitizeHistoryImages("openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}
```

需要的额外 import：`encoding/json` 与 `strings`（`route_test.go` 当前只有 `testing` 与 `prism-proxy/internal/config`，在文件顶部 import 块追加）。

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/route/ -run 'TestSanitize' -v`
Expected: FAIL（`SanitizeHistoryImages undefined`）

- [ ] **Step 3: 实现**

在 `internal/route/route.go` 追加（常量放在文件顶部 `Decision` 结构附近，函数放在 `LatestUserRunHasImage` 之后）：

```go
const (
	analyzedMark = "[image: analyzed in previous reply]"
	omittedMark  = "[image omitted]"
)

// SanitizeHistoryImages 将 run 之前历史消息中的图片块替换为文本标记，
// assistant 解析文本原位保留（语义由历史中相邻的 assistant 回复承载）。
// 必须 map 基遍历：typed 往返会丢弃 cache_control/metadata 等未知字段。
// run 为空或无图 → no-op（返回原 body 字节、sanitized=false）。
func SanitizeHistoryImages(format string, body []byte) ([]byte, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, fmt.Errorf("parse %s request: %w", format, err)
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return nil, false, fmt.Errorf("parse %s request: missing messages", format)
	}
	runStart := 0 // run 为空 → 不遍历（no-op）
	userRoles := map[string]bool{"user": true}
	if format == "openai" {
		userRoles["tool"] = true
	}
	// 与 LatestUserRunHasImage 同一语义：先跳过尾部非用户侧消息，再向前扫 run（中间 assistant 打断）
	end := len(msgs) - 1
	for ; end >= 0; end-- {
		m, ok := msgs[end].(map[string]any)
		if !ok {
			break
		}
		role, _ := m["role"].(string)
		if userRoles[role] {
			break
		}
	}
	for i := end; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			break
		}
		role, _ := m["role"].(string)
		if !userRoles[role] {
			break // 中间的 assistant 打断 run
		}
		runStart = i
	}
	changed := false
	for i := 0; i < runStart; i++ {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		content, has := m["content"]
		if !has {
			continue
		}
		if sanitizeContent(content, hasTextAssistantAfter(msgs, i)) {
			changed = true
		}
	}
	if !changed {
		return body, false, nil // no-op 幂等：逐字节原样
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("marshal %s request: %w", format, err)
	}
	return out, true, nil
}

// sanitizeContent 递归替换 content 中的图片块（image/image_url，含 tool_result 内嵌），
// 其余键原样保留。replay 指示含图消息之后是否有含文本的 assistant 消息（选标记）。
func sanitizeContent(content any, replay bool) bool {
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	changed := false
	for idx, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "image", "image_url":
			mark := analyzedMark
			if !replay {
				mark = omittedMark
			}
			parts[idx] = map[string]any{"type": "text", "text": mark}
			changed = true
		case "tool_result":
			if inner, ok := pm["content"].([]any); ok && sanitizeContent(inner, replay) {
				changed = true
			}
		}
	}
	return changed
}

// hasTextAssistantAfter 扫描 msgs[i] 之后是否存在含非空文本的 assistant 消息。
// 无文本 assistant（如 tool_use）不中断扫描——跨过工具循环，首个含文本的即命中。
func hasTextAssistantAfter(msgs []any, i int) bool {
	for j := i + 1; j < len(msgs); j++ {
		m, ok := msgs[j].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "assistant" {
			continue
		}
		if msgHasText(m["content"]) {
			return true
		}
	}
	return false
}

// msgHasText：消息含非空文本（string 非空，或 []any 中存在非空 text 块）。
func msgHasText(content any) bool {
	switch c := content.(type) {
	case string:
		return c != ""
	case []any:
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if pm["type"] == "text" {
				if t, ok := pm["text"].(string); ok && t != "" {
					return true
				}
			}
		}
	}
	return false
}
```

- [ ] **Step 4: 运行验证通过**

Run: `go test ./internal/route/ -run 'TestSanitize|TestLatestUserRunHasImage|TestDecide' -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/route/route.go internal/route/route_test.go
git commit -m "feat: sanitize history images to text markers before main relay"
```

---

### Task 3: server 挂接 —— 脱敏 + 日志 + 集成测试

**Files:**
- Modify: `internal/server/server.go`（`ServeHTTP` 挂接、`log()` 日志）
- Modify: `internal/server/integration_test.go`（新增集成测试）
- Modify: `internal/route/route.go`（`Decision` 加 `ImagesSanitized bool` 字段）

**Interfaces:**
- Consumes: `route.SanitizeHistoryImages(format, body) ([]byte, bool, error)`（Task 2）、`route.Decision.ImagesSanitized`（本任务新增字段）。
- Produces: `route.Decision.ImagesSanitized bool` — 后续任务无依赖，纯观测。

- [ ] **Step 1: 写失败测试**

在 `internal/server/integration_test.go` 追加：

```go
// TestSanitize_HistoryImageToMain：auto_switch=true + 历史含图 + 最新纯文本 →
// main 收到无图 body（历史图替换为标记），且 model 改写与脱敏共存。
func TestSanitize_HistoryImageToMain(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "image_url") || strings.Contains(string(body), `"image"`) {
			t.Fatalf("image not sanitized: %s", body)
		}
		if !strings.Contains(string(body), "[image: analyzed") {
			t.Fatalf("marker missing: %s", body)
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if m["model"] != "m" {
			t.Fatalf("model not rewritten: %v", m["model"])
		}
		w.Write([]byte(openaiMockResponse("m", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "m"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req := `{"model":"ignored","messages":[
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":"图上有个按钮"},
		{"role":"user","content":"点哪里"}
	]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, body)
	}
}

// TestSanitize_NewImageGoesVision：最新 run 含图 → vision 收到原样 body（不脱敏）。
func TestSanitize_NewImageGoesVision(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "image_url") {
			t.Fatalf("image lost on vision relay: %s", body)
		}
		w.Write([]byte(claudeMockResponse("claude-3", "ok")))
	}))
	defer upstreamSrv.Close()
	cfg := &config.Config{
		AutoSwitchVision: true,
		Upstreams: map[string]config.UpstreamConfig{
			"main": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "openai", Model: "gpt-4o"},
			"vision": {BaseURL: upstreamSrv.URL + "/v1", APIKey: "sk", Format: "claude", Model: "claude-3"},
		},
	}
	srv := NewWithConfig(cfg, upstream.NewClient(), slog.Default())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req := `{"model":"ignored","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, body)
	}
}
```

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/server/ -run 'TestSanitize_' -v`
Expected: FAIL（`route.Decision` 尚无 `ImagesSanitized` 字段——编译错误，或挂接逻辑不存在）

- [ ] **Step 3: 实现**

**3a.** `internal/route/route.go` 的 `Decision` 结构追加字段：

```go
type Decision struct {
	Upstream       string
	Model          string
	VisionSwitch   bool
	HasImage       bool // 入站请求末尾用户侧 run 是否含图（spec §7：命中但不切换时日志标记 image_detected）
	ImagesSanitized bool // 历史图片已被替换为文本标记（仅走 main 且 auto_switch_vision 时可能）
}
```

**3b.** `internal/server/server.go` `ServeHTTP` 中 `route.Decide` 调用之后（现 `route.go:74` 附近）插入：

```go
	if decision.Upstream == "main" && cfg.AutoSwitchVision {
		body, sanitized, err := route.SanitizeHistoryImages(format, body)
		if err != nil {
			writeError(w, format, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		decision.ImagesSanitized = sanitized
	}
```

**3c.** `internal/server/server.go` `log()` 中 `image_detected` 分支之后追加：

```go
	if d.ImagesSanitized {
		attrs = append(attrs, "images_sanitized", true)
	}
```

- [ ] **Step 4: 运行验证通过**

Run: `go test ./...`
Expected: PASS（全部现有 + 新增测试；`TestMatrix_ImageRouting` 的 auto_switch 缺省 false 不受影响）

- [ ] **Step 5: 提交**

```bash
git add internal/route/route.go internal/server/server.go internal/server/integration_test.go
git commit -m "feat: hook history-image sanitize into main relay with logging"
```

---

### Task 4: README 文档 + 全量验证

**Files:**
- Modify: `README.md`

**Interfaces:**
- 无新接口。行为文档化。

- [ ] **Step 1: 更新 README**

`README.md` 功能概述"图片自动切换"条目（约第 10 行）补充语义：

```markdown
- **图片自动切换**：入站请求带图（OpenAI `image_url` / Claude `image`，含 `tool_result` 内嵌）且 `auto_switch_vision: true` 时，自动转发到独立的 `vision` 上游（通常是 Claude），文本请求零变化地走 `main`。
```

替换为：

```markdown
- **图片自动切换**：入站请求**最新轮次**（末尾连续 user/tool 消息段）带图（OpenAI `image_url` / Claude `image`，含 `tool_result` 内嵌）且 `auto_switch_vision: true` 时，自动转发到独立的 `vision` 上游（通常是 Claude），文本请求零变化地走 `main`。
- **历史图片脱敏**：`auto_switch_vision: true` 且走 `main` 时，历史消息中的图片块替换为短文本标记（`[image: analyzed in previous reply]` / `[image omitted]`），模型解析文本在历史中原位保留——纯文本轮次不再触发 vision 切换，也不向纯文本模型发送图片。`auto_switch_vision: false` 时完全原样转发。
```

- [ ] **Step 2: 全量验证**

```bash
go build ./...
go test -race ./...
go test -cover ./...
```

Expected: 构建通过；全部测试 PASS；覆盖率保持 80%+（若因新代码略降，先补足 Task 2 表驱动用例）。

- [ ] **Step 3: 提交**

```bash
git add README.md
git commit -m "docs: document narrowed vision detection and history image sanitize"
```

---

## Self-Review

**Spec 覆盖核对：**

| Spec 要求 | Task |
|---|---|
| §3.1 run 锚点（Claude user / OpenAI user+tool，assistant 打断，assistant 结尾取前段，run 空 → false） | Task 1 |
| §3.2 map 基实现（禁 typed 往返，保 cache_control/tool_result 键） | Task 2（`TestSanitize_CacheControlPreserved`） |
| §3.2 标记 analyzed / omitted（向后扫描含非空 text 的 assistant，跨工具循环） | Task 2（`TestSanitize_ToolLoopFallback` / `TestSanitize_ClaudeNoAssistantText`） |
| §3.2 no-op 幂等（body 逐字节一致） | Task 2（`TestSanitize_NoImageNoop`） |
| §3.3 挂接（main && AutoSwitchVision；vision 不动；false 原样） | Task 3（`TestSanitize_NewImageGoesVision`；`TestMatrix_ImageRouting` 回归） |
| §5 日志 `images_sanitized` | Task 3（3c） |
| §6 交叉格式集成、与 rewriteModel 共存 | Task 3（`TestSanitize_HistoryImageToMain` 断言 model 改写） |
| README 更新 | Task 4 |
| §9 map 基为强制要求 | Global Constraints + Task 2 Step 3 注释 |

**占位符扫描**：无 TBD/TODO；所有步骤含完整代码与命令。

**类型一致性**：`LatestUserRunHasImage`（Task 1）/ `SanitizeHistoryImages`（Task 2）/ `Decision.ImagesSanitized`（Task 3）三处签名全文一致；标记常量 `analyzedMark`/`omittedMark` 与测试字符串字面量一致；`convert.ContentHasImage` 签名与 `messages.go:413` 一致。
