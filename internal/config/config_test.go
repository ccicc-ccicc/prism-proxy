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
		{
			name:    "invalid auth",
			cfg:     &Config{Upstreams: map[string]UpstreamConfig{"main": {BaseURL: "b", APIKey: "k", Format: "openai", Model: "m", Auth: "magic"}}},
			wantErr: "auth must be bearer or x-api-key",
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

func TestLogging_DefaultsAndParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("main:\n  baseurl: http://x/v1\n  api_key: sk\n  format: openai\n  model: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Enabled {
		t.Error("logging.enabled want default false")
	}
	home, _ := os.UserHomeDir()
	wantDir := filepath.Join(home, ".prism-proxy", "logs")
	if cfg.Logging.Dir != wantDir {
		t.Errorf("logging.dir=%q want %q", cfg.Logging.Dir, wantDir)
	}
	if cfg.Logging.MaxFiles != 3 {
		t.Errorf("logging.max_files=%d want 3", cfg.Logging.MaxFiles)
	}
}

func TestLogging_ParseExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("main:\n  baseurl: http://x/v1\n  api_key: sk\n  format: openai\n  model: m\nlogging:\n  enabled: true\n  dir: ~/custom/logs\n  max_files: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Logging.Enabled {
		t.Error("logging.enabled want true")
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, "custom", "logs"); cfg.Logging.Dir != want {
		t.Errorf("dir=%q want %q", cfg.Logging.Dir, want)
	}
	if cfg.Logging.MaxFiles != 5 {
		t.Errorf("max_files=%d want 5", cfg.Logging.MaxFiles)
	}
}
