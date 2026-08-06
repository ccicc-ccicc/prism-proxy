package convert

// Anthropic Messages API 结构。

const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

type MessagesRequest struct {
	Model         string          `json:"model"`
	System        string          `json:"system,omitempty"`
	Messages      []ClaudeMessage `json:"messages"`
	Tools         []ClaudeTool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`
}

type ClaudeBlock struct {
	Type        string       `json:"type"`
	Text        string       `json:"text,omitempty"`
	ID          string       `json:"id,omitempty"`
	Name        string       `json:"name,omitempty"`
	Input       any          `json:"input,omitempty"`
	Content     any          `json:"content,omitempty"`
	ToolUseID   string       `json:"tool_use_id,omitempty"`
	IsError     *bool        `json:"is_error,omitempty"`
	Source      *ImageSource `json:"source,omitempty"`
	Thinking    string       `json:"thinking,omitempty"`
	Signature   string       `json:"signature,omitempty"`
	PartialJSON string       `json:"partial_json,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type ClaudeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// MessagesResponse 的 stop_reason/stop_sequence 不设 omitempty：
// 真实 Anthropic 响应（含 message_start 帧）总是携带这两个字段（值为 null 或字符串）。
type MessagesResponse struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []ClaudeBlock `json:"content"`
	StopReason   *string       `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        *ClaudeUsage  `json:"usage,omitempty"`
	Error        *ClaudeError  `json:"error,omitempty"`
}

type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type ClaudeError struct {
	Type       string `json:"type"`
	Message    string `json:"message"`
	StatusCode int    `json:"status_code,omitempty"`
}

type StreamEvent struct {
	Type         string            `json:"type"`
	Message      *MessagesResponse `json:"message,omitempty"`
	Index        int               `json:"index,omitempty"`
	ContentBlock *ClaudeBlock      `json:"content_block,omitempty"`
	Delta        *ClaudeDelta      `json:"delta,omitempty"`
	Usage        *ClaudeUsage      `json:"usage,omitempty"`
	StopReason   *string           `json:"stop_reason,omitempty"`
	Error        *ClaudeError      `json:"error,omitempty"`
}

// ClaudeDelta 同时服务两类 delta：
//   - content_block_delta：type/text/partial_json（type 恒有值）；
//   - message_delta 的嵌套 delta：stop_reason/stop_sequence（type 缺省，
//     omitempty 保证空 delta 序列化为 {}）。
type ClaudeDelta struct {
	Type         string  `json:"type,omitempty"`
	Text         string  `json:"text,omitempty"`
	PartialJSON  string  `json:"partial_json,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}
