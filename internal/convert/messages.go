package convert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// 原子映射：OpenAI Chat Completions ↔ Claude Messages 的消息/工具/图片/ID 转换。
// ClaudeBlock 的 ToolUseID 字段定义在 claude.go（json tag: tool_use_id）。

// SystemString 将 MessagesRequest.System（string 或文本块数组）规整为纯文本；
// 非 text 块/空值忽略，块间 \n 连接。
func SystemString(system any) string {
	switch s := system.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, p := range s {
			m, ok := p.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text, ok := m["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// OpenAIMessagesToClaude 将 OpenAI 消息序列转为 Claude 消息序列：
//   - system 消息合并进 system 返回值（\n 连接）
//   - assistant 的 tool_calls → tool_use blocks（arguments JSON 串 parse 为对象，非法 → {} + slog 警告）
//   - role=tool → user 消息 + tool_result block
//   - emit 顺序：全部 tool_result 消息先于剩余 user 文本
func OpenAIMessagesToClaude(msgs []ChatMessage) (system string, out []ClaudeMessage, err error) {
	var sys []string
	var pendingText []ClaudeMessage
	waitingResult := false // 已发出 assistant(tool_calls)，等待 tool_result
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sys = append(sys, contentString(m.Content))
		case "assistant":
			flushPending(&out, &pendingText)
			var blocks []ClaudeBlock
			if s := contentString(m.Content); s != "" {
				blocks = append(blocks, ClaudeBlock{Type: "text", Text: s})
			}
			for _, tc := range m.ToolCalls {
				input, perr := OpenAIArgsToClaudeInput(tc.Function.Arguments)
				if perr != nil {
					slog.Warn("tool_call arguments not valid json, using default empty input", "tool", tc.Function.Name)
				}
				blocks = append(blocks, ClaudeBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			out = append(out, ClaudeMessage{Role: "assistant", Content: blocks})
			waitingResult = len(m.ToolCalls) > 0
		case "tool":
			out = append(out, ClaudeMessage{Role: "user", Content: []ClaudeBlock{
				{Type: "tool_result", ToolUseID: m.ToolCallID, Content: contentString(m.Content)},
			}})
			// tool_result 已入列，放行被推迟的 user 文本（tool_result 必须先于剩余文本）
			waitingResult = false
			flushPending(&out, &pendingText)
		case "user":
			if waitingResult {
				// 推迟：Claude 要求 tool_result 紧跟 assistant(tool_use)
				pendingText = append(pendingText, ClaudeMessage{Role: "user", Content: convertOpenAIContent(m.Content)})
				continue
			}
			out = append(out, ClaudeMessage{Role: "user", Content: convertOpenAIContent(m.Content)})
		default:
			return "", nil, fmt.Errorf("unsupported openai role %q", m.Role)
		}
	}
	flushPending(&out, &pendingText)
	return strings.Join(sys, "\n"), out, nil
}

func flushPending(out *[]ClaudeMessage, pending *[]ClaudeMessage) {
	if len(*pending) > 0 {
		*out = append(*out, *pending...)
		*pending = nil
	}
}

// contentString 将消息 content 归一化为字符串（nil → ""，其他形状 JSON 化）。
func contentString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// convertOpenAIContent 将 OpenAI content（string 或 []any 的 ContentPart map）转为
// Claude content。外链图暂用 Source{Type:"url"} 占位，由 upstream 层下载后替换为 base64。
func convertOpenAIContent(c any) any {
	str, ok := c.(string)
	if ok {
		return str
	}
	parts, ok := c.([]any)
	if !ok {
		return contentString(c)
	}
	var blocks []ClaudeBlock
	for _, p := range parts {
		pp, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pp["type"] {
		case "text":
			blocks = append(blocks, ClaudeBlock{Type: "text", Text: fmt.Sprint(pp["text"])})
		case "image_url":
			u, _ := pp["image_url"].(map[string]any)
			urlStr, _ := u["url"].(string)
			src, rest, perr := ImageURLToClaudeSource(urlStr)
			if perr == nil && rest == "" {
				blocks = append(blocks, ClaudeBlock{Type: "image", Source: src})
			} else {
				// 外链图：由调用方（convert/upstream 层）下载后替换；此处先占位标记
				blocks = append(blocks, ClaudeBlock{Type: "image", Source: &ImageSource{Type: "url", Data: urlStr}})
			}
		}
	}
	return blocks
}

// ClaudeMessagesToOpenAI 将 Claude 消息序列转为 OpenAI 消息序列：
//   - system 放 messages[0]（role=system，\n 已由调用方合并）
//   - user/assistant 直转；tool_use block → tool_calls（input 序列化为 JSON 串）
//   - tool_result block → 拆为 role=tool 消息（is_error 前缀错误标记；content 数组转文本）
//   - image block → image_url part（base64 source 构建 data URL，spec §5）；
//     含图消息的 content 输出为 []ContentPart 数组（text + image_url 顺序拼接）
//   - emit 顺序：tool_result（role=tool）消息必须先于同一消息内的 user 文本
func ClaudeMessagesToOpenAI(msgs []ClaudeMessage, system string) ([]ChatMessage, error) {
	var out []ChatMessage
	sys := system
	for _, m := range msgs {
		switch m.Role {
		case "system":
			// Claude Code v2 把系统提示也放入 messages（role=system），
			// 与顶层 system 字段合并，统一置于 messages[0]。
			if t := systemText(m.Content); t != "" {
				if sys != "" {
					sys += "\n"
				}
				sys += t
			}
		case "user", "assistant":
			if s, ok := m.Content.(string); ok {
				// content 可能为字符串（简写）
				out = append(out, ChatMessage{Role: m.Role, Content: s})
				continue
			}
			blocks, ok := contentBlocks(m.Content)
			if !ok {
				out = append(out, ChatMessage{Role: m.Role, Content: contentString(m.Content)})
				continue
			}
			var text strings.Builder
			var imageParts []ContentPart
			var toolCalls []ToolCall
			var toolResults []ChatMessage
			hasImage := false
			for _, bm := range blocks {
				switch bm["type"] {
				case "text":
					text.WriteString(fmt.Sprint(bm["text"]))
				case "image":
					// base64 source → data URL（spec §5 C2O）；source 缺失/非 base64/缺字段
					// 防御性跳过（不 panic、不产生非法 data URL）
					sm, ok := bm["source"].(map[string]any)
					if !ok || sm["type"] != "base64" {
						continue
					}
					mediaType, mok := sm["media_type"].(string)
					data, dok := sm["data"].(string)
					if !mok || !dok || mediaType == "" || data == "" {
						continue
					}
					hasImage = true
					imageParts = append(imageParts, ContentPart{
						Type:     "image_url",
						ImageURL: &ImageURL{URL: ClaudeSourceToImageURL(&ImageSource{Type: "base64", MediaType: mediaType, Data: data})},
					})
				case "tool_use":
					toolCalls = append(toolCalls, ToolCall{
						ID:   fmt.Sprint(bm["id"]),
						Type: "function",
						Function: ToolCallFunction{
							Name:      fmt.Sprint(bm["name"]),
							Arguments: mustJSON(bm["input"]),
						},
					})
				case "tool_result":
					toolResults = append(toolResults, ChatMessage{Role: "tool", ToolCallID: fmt.Sprint(bm["tool_use_id"]), Content: resultToText(bm)})
				case "thinking", "redacted_thinking":
					// 剥离
				}
			}
			// 工具结果必须紧跟 assistant(tool_calls)：toolResults 先入，text 与 tool_calls 排在其后
			out = append(out, toolResults...)
			if hasImage {
				// 含图消息：content 必须是 part 数组（text + image_url）
				content := make([]ContentPart, 0, len(imageParts)+1)
				if text.Len() > 0 {
					content = append(content, ContentPart{Type: "text", Text: text.String()})
				}
				content = append(content, imageParts...)
				if len(content) > 0 {
					out = append(out, ChatMessage{Role: m.Role, Content: content})
				}
			} else if text.Len() > 0 {
				out = append(out, ChatMessage{Role: m.Role, Content: text.String()})
			}
			if len(toolCalls) > 0 {
				out = append(out, ChatMessage{Role: m.Role, Content: "", ToolCalls: toolCalls})
			}
		default:
			return nil, fmt.Errorf("unsupported claude role %q", m.Role)
		}
	}
	if sys != "" {
		out = append([]ChatMessage{{Role: "system", Content: sys}}, out...)
	}
	return out, nil
}

// systemText 提取 role=system 消息的文本：string 直返，块数组取 text 块 \n 连接。
func systemText(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	blocks, ok := contentBlocks(c)
	if !ok {
		return ""
	}
	var parts []string
	for _, bm := range blocks {
		if bm["type"] == "text" {
			if t, ok := bm["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// contentBlocks 将 Claude content 归一化为 map 切片，兼容 []ClaudeBlock 与 []any 两种形状。
func contentBlocks(content any) ([]map[string]any, bool) {
	switch v := content.(type) {
	case []ClaudeBlock:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		var out []map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, false
		}
		return out, true
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, p := range v {
			if bm, ok := p.(map[string]any); ok {
				out = append(out, bm)
			}
		}
		return out, true
	}
	return nil, false
}

// resultToText 将 tool_result block 的 content 归一化为文本（is_error 前缀 [error]）。
func resultToText(bm map[string]any) string {
	switch v := bm["content"].(type) {
	case string:
		if isErr, _ := bm["is_error"].(bool); isErr {
			return "[error] " + v
		}
		return v
	case []any:
		var sb strings.Builder
		for _, c := range v {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := cm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// OpenAIToolsToClaude 将 OpenAI tools 转为 Claude tools（parameters 缺省补 {"type":"object"}）。
func OpenAIToolsToClaude(tools []Tool) []ClaudeTool {
	out := make([]ClaudeTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, ClaudeTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	return out
}

// ClaudeToolsToOpenAI 将 Claude tools 转为 OpenAI tools。
func ClaudeToolsToOpenAI(tools []ClaudeTool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, Tool{Type: "function", Function: FunctionDef{Name: t.Name, Description: t.Description, Parameters: schema}})
	}
	return out
}

// OpenAIArgsToClaudeInput 将 OpenAI tool_call arguments JSON 串 parse 为对象。
// 非法 JSON 返回 map[string]any{} + error，供调用方记日志。
func OpenAIArgsToClaudeInput(args string) (any, error) {
	if args == "" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return map[string]any{}, err
	}
	return v, nil
}

// ClaudeInputToOpenAIArgs 将 Claude tool_use input 序列化为 JSON 串。
func ClaudeInputToOpenAIArgs(input any) (string, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}

// ImageURLToClaudeSource 解析图片 URL：
//   - data URL 解析前缀 → media_type
//   - 非 data URL 返回 ("", url, nil) 表示需下载
func ImageURLToClaudeSource(url string) (*ImageSource, string, error) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil, url, nil
	}
	comma := strings.Index(url, ";base64,")
	if comma < 0 {
		return nil, "", fmt.Errorf("unsupported data url format")
	}
	// media type 取第一个 ";" 前的段：data:image/png;charset=utf-8;base64,... → "image/png"
	mediaType, _, _ := strings.Cut(url[len("data:"):comma], ";")
	if mediaType == "" {
		// spec §5：media_type 与 data 都缺 → 拒绝（400）
		return nil, "", fmt.Errorf("data url missing media type")
	}
	return &ImageSource{Type: "base64", MediaType: mediaType, Data: url[comma+len(";base64,"):]}, "", nil
}

// ClaudeSourceToImageURL 将 ImageSource 序列化为 data URL。
func ClaudeSourceToImageURL(src *ImageSource) string {
	return "data:" + src.MediaType + ";base64," + src.Data
}

// BlockHasImage 检测 blocks 是否含 image block（递归 tool_result.content）。
func BlockHasImage(blocks []ClaudeBlock) bool {
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
		if b.Type == "tool_result" {
			if inner, ok := b.Content.([]ClaudeBlock); ok && BlockHasImage(inner) {
				return true
			}
		}
	}
	return false
}

// ContentHasImage 检测 content 是否含图片，兼容 []ClaudeBlock 与 []any（map）两种形状；
// []any 形态同时识别 Claude 的 "image" block 与 OpenAI 的 "image_url" part，并递归 tool_result。
func ContentHasImage(parts any) bool {
	switch v := parts.(type) {
	case []ClaudeBlock:
		return BlockHasImage(v)
	case []any:
		for _, p := range v {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "image_url", "image":
				return true
			case "tool_result":
				if inner, ok := m["content"].([]any); ok && ContentHasImage(inner) {
					return true
				}
			}
		}
	}
	return false
}
