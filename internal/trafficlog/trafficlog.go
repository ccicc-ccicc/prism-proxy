// Package trafficlog 实现内容日志：JSONL 每请求一条，记录客户端请求、
// 出站请求、上游响应、出站响应四段内容，脱敏并支持大小轮转。
package trafficlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// maxMem 流式/超长内容内存驻留上限：超过则转日志目录临时文件。
const maxMem = 32 << 20 // 32MB

// segBuffer 分段累积缓冲：≤maxMem 驻留内存，超出转目录下临时文件。
// 单 goroutine 使用（每请求一个），无需锁。
type segBuffer struct {
	dir    string
	name   string // 临时文件前缀
	buf    bytes.Buffer
	file   *os.File
	size   int64
	maxMem int
}

func newSegBuffer(dir, name string) *segBuffer {
	return newSegBufferMax(dir, name, maxMem)
}

func newSegBufferMax(dir, name string, maxMem int) *segBuffer {
	return &segBuffer{dir: dir, name: name, maxMem: maxMem}
}

func (s *segBuffer) Write(p []byte) (int, error) {
	if s.file == nil {
		if s.size+int64(len(p)) <= int64(s.maxMem) {
			n, _ := s.buf.Write(p)
			s.size += int64(n)
			return n, nil
		}
		if err := s.spill(); err != nil {
			return 0, err
		}
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	return n, err
}

func (s *segBuffer) spill() error {
	f, err := os.CreateTemp(s.dir, s.name+"-*.part")
	if err != nil {
		return fmt.Errorf("traffic spill: %w", err)
	}
	if s.buf.Len() > 0 {
		if _, err := f.Write(s.buf.Bytes()); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return err
		}
		s.buf.Reset()
	}
	s.file = f
	return nil
}

// String 返回完整内容（内存或临时文件）。
func (s *segBuffer) String() string {
	if s.file == nil {
		return s.buf.String()
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(s.file)
	if err != nil {
		return ""
	}
	return string(data)
}

// Close 关闭并删除临时文件（幂等）。
func (s *segBuffer) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	err := s.file.Close()
	if rmErr := os.Remove(name); err == nil {
		err = rmErr
	}
	s.file = nil
	return err
}
