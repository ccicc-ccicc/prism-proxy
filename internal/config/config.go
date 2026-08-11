package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server           ServerConfig              `yaml:"server"`
	AutoSwitchVision bool                      `yaml:"auto_switch_vision"`
	Logging          LoggingConfig             `yaml:"logging"`
	Upstreams        map[string]UpstreamConfig `yaml:",inline"`
}

// LoggingConfig 内容日志配置（traffic log）。
type LoggingConfig struct {
	Enabled bool   `yaml:"enabled"` // 默认 false
	Dir     string `yaml:"dir"`     // 默认 ~/.prism-proxy/logs，支持 ~ 前缀
	// MaxFiles 轮转保留旧文件数。*int 区分"未设置"与"显式 0"：
	// nil = 未设置（默认 3）；0 = lumberjack 保留全部旧文件（不删除）。
	MaxFiles *int `yaml:"max_files"`
}

// MaxBackups 返回 lumberjack MaxBackups 值：显式 0 时返回 0（保留全部旧文件）；
// 未设置（nil）时返回默认 3。
func (c *LoggingConfig) MaxBackups() int {
	if c.MaxFiles == nil {
		return 3
	}
	return *c.MaxFiles
}

type ServerConfig struct {
	Listen   string   `yaml:"listen"`
	AuthKeys []string `yaml:"auth_keys"`
}

type UpstreamConfig struct {
	BaseURL string        `yaml:"baseurl"`
	APIKey  string        `yaml:"api_key"`
	Format  string        `yaml:"format"`
	Model   string        `yaml:"model"`
	Timeout time.Duration `yaml:"timeout"`
	// Auth 出站认证头，空 = 按 format 默认（openai → Bearer，claude → x-api-key）；
	// 显式 "bearer" 或 "x-api-key" 覆盖。网关类上游（如 AIGW）常用 Bearer 认证。
	Auth string `yaml:"auth"`
}

const defaultTimeout = 120 * time.Second

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8787"
	}
	if c.Logging.Dir == "" {
		c.Logging.Dir = "~/.prism-proxy/logs"
	}
	if c.Logging.MaxFiles == nil {
		def := 3
		c.Logging.MaxFiles = &def
	}
	c.Logging.Dir = expandHome(c.Logging.Dir)
	for name := range c.Upstreams {
		u := c.Upstreams[name]
		if u.Timeout == 0 {
			u.Timeout = defaultTimeout
		}
		c.Upstreams[name] = u
	}
}

// expandHome 展开 ~ 前缀为用户主目录；非 ~ 开头原样返回。
func expandHome(path string) string {
	if path == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[2:])
		}
	}
	return path
}

func (c *Config) Validate() error {
	main, ok := c.Upstreams["main"]
	if !ok {
		return fmt.Errorf("config: upstream 'main' is required")
	}
	if err := validateUpstream("main", main); err != nil {
		return err
	}
	if c.AutoSwitchVision {
		vision, ok := c.Upstreams["vision"]
		if !ok {
			return fmt.Errorf("config: auto_switch_vision is true but upstream 'vision' is missing")
		}
		if err := validateUpstream("vision", vision); err != nil {
			return err
		}
	}
	return nil
}

func validateUpstream(name string, u UpstreamConfig) error {
	if u.BaseURL == "" {
		return fmt.Errorf("config: upstream %q: baseurl is required", name)
	}
	if u.APIKey == "" {
		return fmt.Errorf("config: upstream %q: api_key is required", name)
	}
	if u.Format != "openai" && u.Format != "claude" {
		return fmt.Errorf("config: upstream %q: format must be openai or claude, got %q", name, u.Format)
	}
	if u.Model == "" {
		return fmt.Errorf("config: upstream %q: model is required", name)
	}
	if u.Auth != "" && u.Auth != "bearer" && u.Auth != "x-api-key" {
		return fmt.Errorf("config: upstream %q: auth must be bearer or x-api-key, got %q", name, u.Auth)
	}
	return nil
}

func (c *Config) Main() *UpstreamConfig {
	u := c.Upstreams["main"]
	return &u
}

func (c *Config) Vision() (*UpstreamConfig, bool) {
	u, ok := c.Upstreams["vision"]
	if !ok || u.BaseURL == "" || u.APIKey == "" || u.Model == "" {
		return nil, false
	}
	return &u, true
}
