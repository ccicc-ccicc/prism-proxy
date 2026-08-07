package convert

import "fmt"

const defaultMaxTokens = 4096

func OpenAIRequestToClaude(req *ChatCompletionRequest, model string) (*MessagesRequest, error) {
	if req.N != nil && *req.N > 1 {
		return nil, fmt.Errorf("n>1 not supported")
	}
	maxTokens := defaultMaxTokens
	if req.MaxCompletion != nil && *req.MaxCompletion > 0 {
		maxTokens = *req.MaxCompletion
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	system, msgs, err := OpenAIMessagesToClaude(req.Messages)
	if err != nil {
		return nil, err
	}
	out := &MessagesRequest{
		Model:         model,
		System:        system,
		Messages:      msgs,
		MaxTokens:     maxTokens,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
	}
	if len(req.Tools) > 0 {
		out.Tools = OpenAIToolsToClaude(req.Tools)
	}
	return out, nil
}

func ClaudeRequestToOpenAI(req *MessagesRequest, model string) (*ChatCompletionRequest, error) {
	msgs, err := ClaudeMessagesToOpenAI(req.Messages, SystemString(req.System))
	if err != nil {
		return nil, err
	}
	out := &ChatCompletionRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
	}
	if len(req.Tools) > 0 {
		out.Tools = ClaudeToolsToOpenAI(req.Tools)
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	out.MaxTokens = &maxTokens
	return out, nil
}
