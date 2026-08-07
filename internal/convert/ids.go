package convert

import (
	"crypto/rand"
	"encoding/hex"
)

func newSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func NewOpenAIID() string { return "chatcmpl-" + newSuffix() }
func NewClaudeID() string { return "msg_" + newSuffix() }
