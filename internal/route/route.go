package route

import (
	"encoding/json"
	"fmt"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
)

type Decision struct {
	Upstream     string
	Model        string
	VisionSwitch bool
	HasImage     bool // 入站请求是否含图（spec §7：命中但不切换时日志标记 image_detected）
}

// Decide 按 spec §4 两条规则决策，忽略请求模型名。
func Decide(cfg *config.Config, format string, body []byte) (Decision, error) {
	hasImage, err := LatestUserRunHasImage(format, body)
	if err != nil {
		return Decision{}, err
	}
	if hasImage && cfg.AutoSwitchVision {
		if vision, ok := cfg.Vision(); ok {
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
