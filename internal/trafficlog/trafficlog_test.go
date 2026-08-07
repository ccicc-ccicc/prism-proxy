package trafficlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
