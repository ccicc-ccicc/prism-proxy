package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"prism-proxy/internal/config"
	"prism-proxy/internal/convert"
)

// vision 预处理错误分类（与 forward 上游失败语义一致）：
// errVisionPreprocess 请求侧错误（提取/构造/解析失败）→ 调用方回 400；
// errVisionUpstream vision 上游调用失败（非 2xx/网络/超时）→ 调用方回 502。
var (
	errVisionPreprocess = errors.New("vision preprocess failed")
	errVisionUpstream   = errors.New("vision upstream error")
)

// visionDescribePrompt 固定解析指令：引导 vision 模型详细描述图中内容。
// run 内存在用户文本时追加用户上下文（见 buildVisionRequest）。
const visionDescribePrompt = "请依次详细描述每一张图片的内容：所有可见文本、界面元素、数据、状态。用中文回答。"

// preprocessVision vision 预处理编排：提取最新 run 图片 → 构造 vision 请求
// （只含图 + prompt，绝不带历史）→ 上游单独解析 → 解析文本填回哨兵占位。
// imgs 为空（无 base64 图，如 URL 降级）时跳过 vision 调用，直接返回
// ExtractLatestImages 的 out（URL 图已在提取阶段降级为 [image omitted]）。
func (s *Server) preprocessVision(ctx context.Context, cfg *config.Config, format string, body []byte) ([]byte, error) {
	imgs, userText, out, err := convert.ExtractLatestImages(format, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errVisionPreprocess, err)
	}
	if len(imgs) == 0 {
		return out, nil
	}
	vision, ok := cfg.Vision()
	if !ok {
		return nil, fmt.Errorf("%w: vision upstream not configured", errVisionPreprocess)
	}
	visionReq, err := buildVisionRequest(vision, imgs, userText)
	if err != nil {
		return nil, fmt.Errorf("%w: build vision request: %v", errVisionPreprocess, err)
	}
	resp, err := s.client.Do(ctx, vision, visionReq, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errVisionUpstream, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read vision response: %v", errVisionUpstream, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%w: vision upstream status %d: %s", errVisionUpstream, resp.StatusCode, summarizeBody(data))
	}
	text, err := parseVisionText(vision.Format, data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errVisionPreprocess, err)
	}
	return convert.FillImageTexts(format, out, []string{text})
}

// buildVisionRequest 构造 vision 预处理请求（非流式）：图片块在前、prompt 文本块在后。
// claude 格式 max_tokens 必设（MessagesRequest 无 omitempty，不设序列化 0 会被
// 上游 400 拒绝）；openai 格式图片用 data URL part。
func buildVisionRequest(vision *config.UpstreamConfig, imgs []convert.ImageData, userText string) ([]byte, error) {
	prompt := visionDescribePrompt
	if userText != "" {
		prompt += "结合用户问题「" + userText + "」，重点描述图中相关内容。"
	}
	content := make([]any, 0, len(imgs)+1)
	if vision.Format == "claude" {
		for _, img := range imgs {
			content = append(content, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": img.MediaType,
					"data":       img.Data,
				},
			})
		}
		content = append(content, map[string]any{"type": "text", "text": prompt})
		return json.Marshal(map[string]any{
			"model":      vision.Model,
			"max_tokens": 1024,
			"messages":   []any{map[string]any{"role": "user", "content": content}},
		})
	}
	for _, img := range imgs {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + img.MediaType + ";base64," + img.Data},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})
	return json.Marshal(map[string]any{
		"model":    vision.Model,
		"messages": []any{map[string]any{"role": "user", "content": content}},
	})
}

// parseVisionText 提取 vision 上游响应的解析文本：
//   - openai：choices[0].message.content——string 直接用；数组拼接全部 text part；
//     null/空 或 choices 空 → error（400）；
//   - claude：content 块数组拼接 text 块（thinking/tool_use 跳过）；无 text → error。
func parseVisionText(format string, data []byte) (string, error) {
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse vision response: %w", err)
	}
	if format == "claude" {
		blocks, _ := resp["content"].([]any)
		var sb strings.Builder
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok || bm["type"] != "text" {
				continue
			}
			if t, ok := bm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		if sb.Len() == 0 {
			return "", fmt.Errorf("vision response has no text block")
		}
		return sb.String(), nil
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return "", fmt.Errorf("vision response has no choices")
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("vision response malformed choices")
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("vision response missing message")
	}
	switch content := msg["content"].(type) {
	case string:
		if content == "" {
			return "", fmt.Errorf("vision response has empty content")
		}
		return content, nil
	case []any:
		var sb strings.Builder
		for _, p := range content {
			pm, ok := p.(map[string]any)
			if !ok || pm["type"] != "text" {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		if sb.Len() == 0 {
			return "", fmt.Errorf("vision response has no text part")
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("vision response content is not text")
}

// summarizeBody 截断上游错误体为错误信息摘要（防大 body 撑爆错误信封/日志）。
func summarizeBody(data []byte) string {
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
