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

type MessagesResponse struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Role       string        `json:"role"`
	Model      string        `json:"model"`
	Content    []ClaudeBlock `json:"content"`
	StopReason *string       `json:"stop_reason,omitempty"`
	Usage      *ClaudeUsage  `json:"usage,omitempty"`
	Error      *ClaudeError  `json:"error,omitempty"`
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

type ClaudeDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}
