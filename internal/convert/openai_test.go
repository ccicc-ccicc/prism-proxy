package convert

import (
	"encoding/json"
	"testing"
)

func TestOpenAIRequestUnmarshal(t *testing.T) {
	data := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}]}`)
	var req ChatCompletionRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o" || !req.Stream {
		t.Fatal("model/stream")
	}
	parts, ok := req.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts: %#v", req.Messages[0].Content)
	}
	if req.Tools[0].Function.Name != "f" {
		t.Fatal("tool")
	}
}
