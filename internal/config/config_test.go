package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	path := writeTemp(t, `
server:
  listen: ":8787"
  auth_keys: ["sk-proxy-1"]
auto_switch_vision: true
main:
  baseurl: "https://api.openai.com/v1"
  api_key: "sk-main"
  format: openai
  model: "gpt-4o"
vision:
  baseurl: "https://vision.example.com/v1"
  api_key: "sk-vision"
  format: claude
  model: "gpt-4o-vision"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoSwitchVision || len(cfg.Server.AuthKeys) != 1 {
		t.Fatalf("fields: %+v", cfg.Server)
	}
	if cfg.Main().Model != "gpt-4o" {
		t.Fatalf("main: %+v", cfg.Main())
	}
	if v, ok := cfg.Vision(); !ok || v.Format != "claude" {
		t.Fatalf("vision: %+v ok=%v", v, ok)
	}
	if cfg.Main().Timeout != 120*time.Second {
		t.Fatalf("default timeout: %v", cfg.Main().Timeout)
	}
}

func TestValidateMainMissing(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing main")
	}
}

func TestValidateVisionRequiredButMissing(t *testing.T) {
	cfg := &Config{AutoSwitchVision: true}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: vision required when auto_switch_vision on")
	}
}

func TestValidateUpstreamFieldRules(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name:    "missing baseurl",
			cfg:     &Config{Upstreams: map[string]UpstreamConfig{"main": {APIKey: "k", Format: "openai", Model: "m"}}},
			wantErr: "baseurl",
		},
		{
			name:    "missing api_key",
			cfg:     &Config{Upstreams: map[string]UpstreamConfig{"main": {BaseURL: "b", Format: "openai", Model: "m"}}},
			wantErr: "api_key",
		},
		{
			name:    "invalid format",
			cfg:     &Config{Upstreams: map[string]UpstreamConfig{"main": {BaseURL: "b", APIKey: "k", Format: "ollama", Model: "m"}}},
			wantErr: "format must be openai or claude",
		},
		{
			name:    "missing model",
			cfg:     &Config{Upstreams: map[string]UpstreamConfig{"main": {BaseURL: "b", APIKey: "k", Format: "openai"}}},
			wantErr: "model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error for %q", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}
