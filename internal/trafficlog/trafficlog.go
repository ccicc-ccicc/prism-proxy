// Package trafficlog 实现内容日志：JSONL 每请求一条，记录客户端请求、
// 出站请求、上游响应、出站响应四段内容，脱敏并支持大小轮转。
package trafficlog

import (
	"encoding/json"
)

// Redact 将 JSON body 中 api_key / key 字段的字符串值替换为 sk-***。
// 非 JSON body 原样返回；JSON 字段名精确匹配（含嵌套对象与数组）。
func Redact(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "api_key" || k == "key" {
				if _, ok := val.(string); ok {
					t[k] = "sk-***"
				}
			} else {
				redactValue(val)
			}
		}
	case []any:
		for _, e := range t {
			redactValue(e)
		}
	}
}
