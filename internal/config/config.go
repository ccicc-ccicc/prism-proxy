package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server           ServerConfig              `yaml:"server"`
	AutoSwitchVision bool                      `yaml:"auto_switch_vision"`
	Upstreams        map[string]UpstreamConfig `yaml:",inline"`
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
	for name := range c.Upstreams {
		u := c.Upstreams[name]
		if u.Timeout == 0 {
			u.Timeout = defaultTimeout
		}
		c.Upstreams[name] = u
	}
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
