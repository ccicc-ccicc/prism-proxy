package route

import (
	"encoding/json"
	"fmt"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
)

const (
	analyzedMark = "[image: analyzed in previous reply]"
	omittedMark  = "[image omitted]"
)

type Decision struct {
	Upstream           string
	Model              string
	VisionSwitch       bool
	HasImage           bool  // 入站请求末尾用户侧 run 是否含图（spec §7：命中但不切换时日志标记 image_detected）
	ImagesSanitized    bool  // 历史图片已被替换为文本标记（仅走 main 且 auto_switch_vision 时可能）
	NeedPreprocess     bool  // vision 预处理模式：最新 run 含图，需 vision 解析后走 main
	VisionPreprocess   bool  // 本次请求实际执行了 vision 预处理（日志标记）
	VisionPreprocessMs int64 // 预处理耗时毫秒数（日志标记）
}

// Decide 按 spec §4 两条规则决策，忽略请求模型名。
func Decide(cfg *config.Config, format string, body []byte) (Decision, error) {
	hasImage, err := LatestUserRunHasImage(format, body)
	if err != nil {
		return Decision{}, err
	}
	if hasImage && cfg.AutoSwitchVision {
		if vision, ok := cfg.Vision(); ok {
			if cfg.VisionPreprocess {
				// 预处理模式：最新图经 vision 解析后走 main（不切上游）
				return Decision{Upstream: "main", Model: cfg.Main().Model, HasImage: true, NeedPreprocess: true}, nil
			}
			return Decision{Upstream: "vision", Model: vision.Model, VisionSwitch: true, HasImage: true}, nil
		}
	}
	return Decision{Upstream: "main", Model: cfg.Main().Model, HasImage: hasImage}, nil
}

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
			break // assistant 打断 run
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

// hasTextAssistantAfter 扫描 msgs[i] 之后是否存在含非空文本的 assistant 消息
// （跨过 tool_use 循环中间消息：无文本 assistant 不中断扫描，继续向后找）。
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
