package trafficlog

import (
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
