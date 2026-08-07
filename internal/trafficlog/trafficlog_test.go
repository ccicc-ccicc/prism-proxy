package trafficlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedact_ApiKeyNested(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"api_key":"sk-secret-123"}]}`)
	out := Redact(in)
	if strings.Contains(string(out), "sk-secret-123") {
		t.Fatalf("leaked api_key: %s", out)
	}
	if !strings.Contains(string(out), "sk-***") {
		t.Fatalf("no mask: %s", out)
	}
}

func TestRedact_KeyField(t *testing.T) {
	out := Redact([]byte(`{"key":"abc123","name":"x"}`))
	if strings.Contains(string(out), "abc123") {
		t.Fatalf("leaked key: %s", out)
	}
	if !strings.Contains(string(out), "sk-***") {
		t.Fatalf("no mask: %s", out)
	}
}

func TestRedact_NonJSON(t *testing.T) {
	in := []byte("not json at all")
	if string(Redact(in)) != string(in) {
		t.Fatalf("non-json changed: %s", Redact(in))
	}
}

func TestRedact_ArrayAndNormalFields(t *testing.T) {
	in := []byte(`[{"key":"k1","content":"keep me"},{"nested":{"api_key":"sk-x"}}]`)
	out := Redact(in)
	s := string(out)
	if strings.Contains(s, "k1") || strings.Contains(s, "sk-x") {
		t.Fatalf("leaked: %s", s)
	}
	if !strings.Contains(s, "keep me") {
		t.Fatalf("normal content lost: %s", s)
	}
}

func TestSegBuffer_InMemory(t *testing.T) {
	s := newSegBufferMax(t.TempDir(), "seg", 1024)
	defer s.Close()
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if s.String() != "hello" {
		t.Fatalf("got %q", s.String())
	}
	if s.file != nil {
		t.Fatal("want in-memory, got file")
	}
}

func TestSegBuffer_SpillToDisk(t *testing.T) {
	dir := t.TempDir()
	s := newSegBufferMax(dir, "seg", 8)
	defer s.Close()
	big := bytes.Repeat([]byte("a"), 100)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if s.file == nil {
		t.Fatal("want spilled to file")
	}
	if s.String() != string(big) {
		t.Fatalf("content mismatch: len %d want %d", len(s.String()), len(big))
	}
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".part" {
			found = true
		}
	}
	if !found {
		t.Fatal("no .part temp file")
	}
}

func TestSegBuffer_CloseRemovesFile(t *testing.T) {
	dir := t.TempDir()
	s := newSegBufferMax(dir, "seg", 8)
	_, _ = s.Write(bytes.Repeat([]byte("b"), 100))
	name := s.file.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed: %v", err)
	}
}

func TestTrafficLog_WriteEntry(t *testing.T) {
	var buf bytes.Buffer
	tl := NewWithWriter(&buf)
	e := Entry{
		TS: "2026-08-07T10:00:00+08:00", RequestID: "rid1", Inbound: "openai",
		Upstream: "main", Outbound: "claude", Model: "m", Stream: false,
		Status: 200, DurationMS: 123, InboundBody: `{"a":1}`,
		UpstreamResponse: `{"b":2}`, Error: "",
	}
	if err := tl.WriteEntry(e); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatal(err)
	}
	if got["request_id"] != "rid1" || got["status"] != float64(200) {
		t.Fatalf("entry: %s", lines[0])
	}
	if got["inbound_body"] != `{"a":1}` {
		t.Fatalf("inbound_body: %v", got["inbound_body"])
	}
}

func TestTrafficLog_ConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	tl := NewWithWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = tl.WriteEntry(Entry{RequestID: "r" + string(rune('0'+n)), TS: "t"})
		}(i % 10)
	}
	wg.Wait()
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
}

func TestTrafficLog_NewCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	tl, err := New(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestSegBuffer_SpillThenContinue(t *testing.T) {
	dir := t.TempDir()
	s := newSegBufferMax(dir, "seg", 8)
	defer s.Close()
	big := bytes.Repeat([]byte("a"), 100)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	want := string(big) + "tail"
	if s.String() != want {
		t.Fatalf("spill then write: got len %d want %d", len(s.String()), len(want))
	}
}

func TestSegBuffer_SpillFailKeepsData(t *testing.T) {
	s := newSegBufferMax(filepath.Join(t.TempDir(), "nope"), "seg", 8)
	defer s.Close()
	_, err := s.Write(bytes.Repeat([]byte("c"), 100))
	if err == nil {
		t.Fatal("want spill error")
	}
	if s.file != nil {
		t.Fatal("file should stay nil on failure")
	}
	// 数据保留在内存，换到可写目录后仍可成功落盘
	if s.buf.Len() != 100 {
		t.Fatalf("buf len=%d want 100", s.buf.Len())
	}
}

func TestRecorder_EntryAssembly(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid-1", "openai")
	defer rec.Close()
	rec.SetDecision("main", "claude", "claude-3")
	rec.SetInbound([]byte(`{"api_key":"sk-secret","messages":[{"role":"user","content":"hi"}]}`))
	rec.SetOutbound([]byte(`{"model":"claude-3"}`))
	rec.SetUpstreamResponse([]byte(`{"id":"m1"}`))
	rec.SetOutboundResponse([]byte(`{"id":"c1"}`))
	rec.SetStatus(200)
	entry := rec.Entry(false, 500*time.Millisecond)
	if entry.RequestID != "rid-1" || entry.Inbound != "openai" || entry.Upstream != "main" || entry.Outbound != "claude" || entry.Model != "claude-3" {
		t.Fatalf("meta: %+v", entry)
	}
	if entry.DurationMS != 500 {
		t.Fatalf("duration_ms=%d want 500", entry.DurationMS)
	}
	if !strings.Contains(entry.InboundBody, "sk-***") || strings.Contains(entry.InboundBody, "sk-secret") {
		t.Fatalf("inbound not redacted: %s", entry.InboundBody)
	}
	if entry.UpstreamResponse != `{"id":"m1"}` {
		t.Fatalf("upstream_response: %s", entry.UpstreamResponse)
	}
}

func TestRecorder_StreamWriters(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid-2", "claude")
	defer rec.Close()
	rec.SetDecision("main", "openai", "gpt-4o")
	rec.SetStatus(200)
	_, _ = rec.UpstreamWriter().Write([]byte("data: {\"a\":1}\n\ndata: [DONE]\n"))
	_, _ = rec.OutboundWriter().Write([]byte("event: message_start\n\nevent: message_stop\n"))
	entry := rec.Entry(true, 10*time.Millisecond)
	if entry.UpstreamResponse != "data: {\"a\":1}\n\ndata: [DONE]\n" {
		t.Fatalf("upstream stream: %q", entry.UpstreamResponse)
	}
	if entry.OutboundResponse != "event: message_start\n\nevent: message_stop\n" {
		t.Fatalf("outbound stream: %q", entry.OutboundResponse)
	}
}

func TestRecorder_OutboundFallbackToUpstream(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid-3", "openai")
	defer rec.Close()
	rec.SetDecision("main", "openai", "gpt-4o")
	rec.SetUpstreamResponse([]byte(`{"choices":[]}`))
	entry := rec.Entry(false, 0)
	if entry.OutboundResponse != `{"choices":[]}` {
		t.Fatalf("outbound fallback: %q", entry.OutboundResponse)
	}
}

func TestRecorder_SetErrorFirstWins(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid", "openai")
	defer rec.Close()
	rec.SetError(errors.New("inner"))
	rec.SetError(errors.New("outer"))
	if rec.Entry(false, 0).Error != "inner" {
		t.Fatalf("want first error, got %q", rec.Entry(false, 0).Error)
	}
}

func TestRecorder_EntrySpillContent(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, "rid", "openai")
	defer rec.Close()
	big := bytes.Repeat([]byte("x"), 40<<20) // 40MB 触发落盘
	rec.SetOutboundResponse(big)
	entry := rec.Entry(false, 0)
	if entry.OutboundResponse != string(big) {
		t.Fatalf("spill content mismatch: len %d want %d", len(entry.OutboundResponse), len(big))
	}
}
