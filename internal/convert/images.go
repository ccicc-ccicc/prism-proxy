package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImageData 提取的图片数据（base64）。
type ImageData struct {
	MediaType string
	Data      string
}

// imagePendingSentinel 哨兵占位：含不可打印控制字符，用户文本不可能出现
// （避免与真实文本冲突）。提取图块原位替换为文本块，FillImageTexts 依次回填。
const imagePendingSentinel = "\x00prism-image-pending"

// imageOmittedMark URL 来源图块降级标记（与 route.omittedMark 同文案）。
const imageOmittedMark = "[image omitted]"

// ExtractLatestImages 提取最新 run 内所有图片块（含 tool_result 内嵌），
// 图块原位替换为哨兵占位 text 块（顺序对应 imgs），同时提取 run 内用户文本
// （user 消息 text，排除 tool_result 块、排除图块本身）。
// run 边界与 route.SanitizeHistoryImages/LatestUserRunHasImage 完全一致：
// 末尾连续用户侧消息段（claude: role=user；openai: role=user|tool），
// 任何 assistant（含 tool_use）打断。即只提取"最后一段用户侧消息"的图。
// URL 来源（claude source.type=url / openai 非 data URL）→ 不入 imgs，
// 图块直接替换为 [image omitted]（文档化降级）。
// 无图（run 内无任何图）→ (nil, "", 原 body 字节, nil)。
func ExtractLatestImages(format string, body []byte) (imgs []ImageData, userText string, out []byte, err error) {
	var root map[string]any
	if err = json.Unmarshal(body, &root); err != nil {
		return nil, "", nil, fmt.Errorf("parse %s request: %w", format, err)
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return nil, "", nil, fmt.Errorf("parse %s request: missing messages", format)
	}
	userRoles := map[string]bool{"user": true}
	if format == "openai" {
		userRoles["tool"] = true
	}
	// run 边界与 SanitizeHistoryImages 同一语义：先跳过尾部非用户侧消息，再向前扫 run
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
	runStart := 0
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
	var texts []string
	for i := runStart; i <= end; i++ {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if !userRoles[role] {
			continue // 防御：畸形尾部下的越界消息
		}
		content, has := m["content"]
		if !has {
			continue
		}
		if role == "user" {
			texts = append(texts, extractUserText(content)...)
		}
		extractContentImages(content, &imgs, &changed)
	}
	if !changed {
		return nil, "", body, nil // no-op：逐字节原样
	}
	out, err = json.Marshal(root)
	if err != nil {
		return nil, "", nil, fmt.Errorf("marshal %s request: %w", format, err)
	}
	return imgs, strings.Join(texts, "\n"), out, nil
}

// extractUserText 提取 content 中用户文本：string 内容或 text 块；
// tool_result/image/image_url 块跳过（其内容非用户输入）。
func extractUserText(content any) []string {
	var out []string
	switch c := content.(type) {
	case string:
		if c != "" {
			out = append(out, c)
		}
	case []any:
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if pm["type"] == "text" {
				if t, ok := pm["text"].(string); ok && t != "" {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// extractContentImages 递归处理 content 数组：提取 image/image_url 图片
// （图块原位替换为哨兵占位或 [image omitted]），tool_result 内嵌递归。
func extractContentImages(content any, imgs *[]ImageData, changed *bool) {
	parts, ok := content.([]any)
	if !ok {
		return
	}
	for idx, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "image":
			if img, ok := extractClaudeImage(pm); ok {
				*imgs = append(*imgs, *img)
				parts[idx] = map[string]any{"type": "text", "text": imagePendingSentinel}
			} else {
				parts[idx] = map[string]any{"type": "text", "text": imageOmittedMark}
			}
			*changed = true
		case "image_url":
			if img, ok := extractOpenAIImageURL(pm); ok {
				*imgs = append(*imgs, *img)
				parts[idx] = map[string]any{"type": "text", "text": imagePendingSentinel}
			} else {
				parts[idx] = map[string]any{"type": "text", "text": imageOmittedMark}
			}
			*changed = true
		case "tool_result":
			extractContentImages(pm["content"], imgs, changed)
		}
	}
}

// extractClaudeImage 从 claude image block 提取 base64 图数据。
// source.type=base64 且 media_type/data 非空 → (img, true)；
// url 或其他不可解析来源 → (nil, false)（调用方降级 [image omitted]）。
func extractClaudeImage(pm map[string]any) (*ImageData, bool) {
	src, ok := pm["source"].(map[string]any)
	if !ok {
		return nil, false
	}
	if src["type"] != "base64" {
		return nil, false
	}
	media, _ := src["media_type"].(string)
	data, _ := src["data"].(string)
	if media == "" || data == "" {
		return nil, false
	}
	return &ImageData{MediaType: media, Data: data}, true
}

// extractOpenAIImageURL 解析 openai image_url part：data URL → base64 图；
// 非 data URL 或解析失败 → (nil, false)（调用方降级 [image omitted]）。
func extractOpenAIImageURL(pm map[string]any) (*ImageData, bool) {
	iu, ok := pm["image_url"].(map[string]any)
	if !ok {
		return nil, false
	}
	url, _ := iu["url"].(string)
	if url == "" {
		return nil, false
	}
	src, _, err := ImageURLToClaudeSource(url)
	if err != nil || src == nil {
		return nil, false
	}
	return &ImageData{MediaType: src.MediaType, Data: src.Data}, true
}

// FillImageTexts 将 body 中的哨兵占位依次替换为
// {"type":"text","text":"[图片内容: <text>]"}；texts 长度不足时余下占位
// 替换为 [image omitted]。无占位 → 原 body 字节。
func FillImageTexts(format string, body []byte, texts []string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("parse %s request: %w", format, err)
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("parse %s request: missing messages", format)
	}
	idx := 0
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		fillContentTexts(mm["content"], texts, &idx, &changed)
	}
	if !changed {
		return body, nil // no-op：逐字节原样
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", format, err)
	}
	return out, nil
}

// fillContentTexts 递归替换 content 中的哨兵占位（tool_result 内嵌递归）。
// 原地改写 text 块 text 字段，保留其余字段（如 cache_control）。
func fillContentTexts(content any, texts []string, idx *int, changed *bool) {
	parts, ok := content.([]any)
	if !ok {
		return
	}
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "text":
			if t, ok := pm["text"].(string); ok && t == imagePendingSentinel {
				fill := imageOmittedMark
				if *idx < len(texts) {
					fill = "[图片内容: " + texts[*idx] + "]"
					*idx++
				}
				pm["text"] = fill
				*changed = true
			}
		case "tool_result":
			fillContentTexts(pm["content"], texts, idx, changed)
		}
	}
}
