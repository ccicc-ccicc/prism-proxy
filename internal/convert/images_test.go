package convert

import (
	"encoding/json"
	"fmt"
	"testing"
)

// sentinelJSON 哨兵的 JSON 字符串字面量：控制字符经 json.Marshal 转义为
// unicode 转义序列（%q 的 \x 转义不是合法 JSON，不能直接用于构造测试 body）。
func sentinelJSON() string {
	b, err := json.Marshal(imagePendingSentinel)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestExtractLatestImages_ClaudeExtractAndPlaceholder(t *testing.T) {
	// 历史含图（assistant 文本打断 run）+ 最新 run 含图 → 只提取最新 run 图块，
	// 图块原位替换为哨兵占位，历史图原样保留。
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"HIST"}}]},
		{"role":"assistant","content":"好的"},
		{"role":"user","content":[{"type":"text","text":"看"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}]}
	]}`)
	imgs, userText, out, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("imgs: %+v", imgs)
	}
	if imgs[0].MediaType != "image/jpeg" || imgs[0].Data != "AAAA" {
		t.Fatalf("img: %+v", imgs[0])
	}
	if userText != "看" {
		t.Fatalf("userText: %q", userText)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	runBlocks := msgs[2].(map[string]any)["content"].([]any)
	if len(runBlocks) != 2 {
		t.Fatalf("run blocks: %v", runBlocks)
	}
	ph := runBlocks[1].(map[string]any)
	if ph["type"] != "text" || ph["text"] != imagePendingSentinel {
		t.Fatalf("placeholder: %v", ph)
	}
	if runBlocks[0].(map[string]any)["text"] != "看" {
		t.Fatalf("text part mutated: %v", runBlocks[0])
	}
	// 历史图不被提取、不被替换
	hist := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if hist["type"] != "image" {
		t.Fatalf("history image mutated: %v", hist)
	}
	src := hist["source"].(map[string]any)
	if src["data"] != "HIST" {
		t.Fatalf("history image data: %v", src)
	}
}

func TestExtractLatestImages_ToolResultNested(t *testing.T) {
	// tool_result 内嵌 image block（Claude 工具循环截图场景）→ 递归提取并替换
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"TOOLIMG"}}]}]}
	]}`)
	imgs, _, out, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].MediaType != "image/webp" || imgs[0].Data != "TOOLIMG" {
		t.Fatalf("imgs: %+v", imgs)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	inner := m["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)
	if len(inner) != 1 || inner[0].(map[string]any)["text"] != imagePendingSentinel {
		t.Fatalf("nested placeholder: %v", inner)
	}
}

func TestExtractLatestImages_RunBoundaryToolUseInterrupt(t *testing.T) {
	// 工具循环：assistant(tool_use) 打断 run（与 SanitizeHistoryImages 一致）——
	// tool_use 之前的 user 图属历史 run 不提取；tool_result 之后的连续 user 侧消息才是当前 run。
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"text","text":"截图A"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"HIST"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"screenshot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"RUN"}}]}]},
		{"role":"user","content":"看这个"}
	]}`)
	imgs, userText, out, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].Data != "RUN" || imgs[0].MediaType != "image/jpeg" {
		t.Fatalf("imgs: %+v", imgs)
	}
	if userText != "看这个" {
		t.Fatalf("userText: %q", userText)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	// tool_use 之前的图属历史 run：原样保留
	hist := msgs[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if hist["type"] != "image" {
		t.Fatalf("history image mutated: %v", hist)
	}
	// tool_result 内嵌图（当前 run）→ 哨兵占位
	inner := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)
	if len(inner) != 1 || inner[0].(map[string]any)["text"] != imagePendingSentinel {
		t.Fatalf("run placeholder: %v", inner)
	}
}

func TestExtractLatestImages_URLOmitted(t *testing.T) {
	// claude source.type=url → 不入 imgs，图块替换为 [image omitted]；base64 图照常提取
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://x.com/a.png"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"OK"}}
		]}
	]}`)
	imgs, _, out, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].Data != "OK" {
		t.Fatalf("imgs: %+v", imgs)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "[image omitted]" {
		t.Fatalf("url degraded block: %v", blocks[0])
	}
	if blocks[1].(map[string]any)["text"] != imagePendingSentinel {
		t.Fatalf("base64 block: %v", blocks[1])
	}

	// openai 非 data URL → 同样降级（不入 imgs）
	body2 := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.com/a.png"}}]}]}`)
	imgs2, _, out2, err := ExtractLatestImages("openai", body2)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs2) != 0 {
		t.Fatalf("imgs2: %+v", imgs2)
	}
	var m2 map[string]any
	_ = json.Unmarshal(out2, &m2)
	blocks2 := m2["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks2[0].(map[string]any)["text"] != "[image omitted]" {
		t.Fatalf("openai url degraded block: %v", blocks2[0])
	}
}

func TestExtractLatestImages_UserTextExclusions(t *testing.T) {
	// userText：排除 tool_result 块内容、排除图块本身、多条 user 消息 \n 拼接
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"结果X"},{"type":"text","text":"第一段"}]},
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A"}}]},
		{"role":"user","content":"第二段"}
	]}`)
	_, userText, _, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if userText != "第一段\n第二段" {
		t.Fatalf("userText: %q", userText)
	}
}

func TestExtractLatestImages_MultiImageOrder(t *testing.T) {
	// 多图顺序：imgs[0]/imgs[1] 与占位顺序一一对应
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMG0"}},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"IMG1"}}
		]}
	]}`)
	imgs, _, out, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 || imgs[0].Data != "IMG0" || imgs[1].Data != "IMG1" {
		t.Fatalf("imgs: %+v", imgs)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	for i, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] != "text" || bm["text"] != imagePendingSentinel {
			t.Fatalf("block[%d]: %v", i, bm)
		}
	}
}

func TestExtractLatestImages_OpenAIImageURL(t *testing.T) {
	// openai：image_url data URL part → base64 提取；text 保留
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,URLIMG"}}]}
	]}`)
	imgs, userText, out, err := ExtractLatestImages("openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].MediaType != "image/png" || imgs[0].Data != "URLIMG" {
		t.Fatalf("imgs: %+v", imgs)
	}
	if userText != "看图" {
		t.Fatalf("userText: %q", userText)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	parts := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[1].(map[string]any)["text"] != imagePendingSentinel {
		t.Fatalf("placeholder: %v", parts[1])
	}
}

func TestExtractLatestImages_OpenAIToolCallsAssistant(t *testing.T) {
	// 工具循环：assistant(tool_calls) 在 run 起点打断；role=tool 消息是 run 成员，
	// 其 image_url part 被提取；user 消息文本进 userText（tool 消息文本不收集）。
	body := []byte(`{"model":"x","messages":[
		{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"screenshot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,TOOLIMG"}}]},
		{"role":"user","content":"看这个"}
	]}`)
	imgs, userText, _, err := ExtractLatestImages("openai", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].Data != "TOOLIMG" {
		t.Fatalf("imgs: %+v", imgs)
	}
	if userText != "看这个" {
		t.Fatalf("userText: %q", userText)
	}
}

func TestExtractLatestImages_NoImageNoop(t *testing.T) {
	// 无图 → 原 body 逐字节返回
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	imgs, userText, out, err := ExtractLatestImages("claude", body)
	if err != nil {
		t.Fatal(err)
	}
	if imgs != nil || userText != "" {
		t.Fatalf("imgs=%+v userText=%q", imgs, userText)
	}
	if string(out) != string(body) {
		t.Fatalf("body not byte-identical:\n%s", out)
	}
}

func TestExtractLatestImages_InvalidBody(t *testing.T) {
	_, _, _, err := ExtractLatestImages("openai", []byte("{not json"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFillImageTexts_Replace(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":%s}]}]}`, sentinelJSON()))
	out, err := FillImageTexts("claude", body, []string{"图中是登录页"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "[图片内容: 图中是登录页]" {
		t.Fatalf("missing filled text: %s", out)
	}
}

func TestFillImageTexts_InsufficientTexts(t *testing.T) {
	// texts 长度不足 → 余下占位替换为 [image omitted]
	body := []byte(fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":[
		{"type":"text","text":%s},
		{"type":"text","text":%s}
	]}]}`, sentinelJSON(), sentinelJSON()))
	out, err := FillImageTexts("claude", body, []string{"只有一个"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "[图片内容: 只有一个]" {
		t.Fatalf("missing first fill: %s", out)
	}
	if blocks[1].(map[string]any)["text"] != "[image omitted]" {
		t.Fatalf("missing omitted: %s", out)
	}
}

func TestFillImageTexts_NestedToolResult(t *testing.T) {
	// tool_result 内嵌占位（对应嵌套提取的图）也要被填充
	body := []byte(fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":%s}]}
	]}]}`, sentinelJSON()))
	out, err := FillImageTexts("claude", body, []string{"截图中是仪表盘"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	inner := m["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)
	if inner[0].(map[string]any)["text"] != "[图片内容: 截图中是仪表盘]" {
		t.Fatalf("nested fill missing: %s", out)
	}
}

func TestFillImageTexts_NoPlaceholderNoop(t *testing.T) {
	// 无占位 → 原 body 逐字节返回
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, err := FillImageTexts("openai", body, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("body not byte-identical:\n%s", out)
	}
}

func TestFillImageTexts_InvalidBody(t *testing.T) {
	_, err := FillImageTexts("openai", []byte("{not json"), nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
