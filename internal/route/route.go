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
	hasImage, err := RequestHasImage(format, body)
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

// RequestHasImage 结构化检测入站请求是否含图片（递归 tool_result）。
func RequestHasImage(format string, body []byte) (bool, error) {
	switch format {
	case "openai":
		var req convert.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return false, fmt.Errorf("parse openai request: %w", err)
		}
		for _, m := range req.Messages {
			if parts, ok := m.Content.([]any); ok {
				for _, p := range parts {
					if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
						return true, nil
					}
				}
			}
		}
		return false, nil
	case "claude":
		var req convert.MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return false, fmt.Errorf("parse claude request: %w", err)
		}
		for _, m := range req.Messages {
			if blocks, ok := m.Content.([]convert.ClaudeBlock); ok {
				if convert.BlockHasImage(blocks) {
					return true, nil
				}
			} else if parts, ok := m.Content.([]any); ok {
				if convert.ContentHasImage(parts) {
					return true, nil
				}
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unknown format %q", format)
	}
}
