package main

import (
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Checking  CheckingConfig    `toml:"checking"`
	Cookies   map[string]string `toml:"cookies"`
	Filtering FilteringConfig   `toml:"filtering"`
}

type CheckingConfig struct {
	SSLVerify bool   `toml:"ssl_verify"`
	Threads   int    `toml:"threads"`
	MaxDepth  int    `toml:"max_depth"`
	Timeout   string `toml:"timeout"`
}

type IgnoreUnlessRule struct {
	Pattern string `toml:"pattern"`
	Unless  string `toml:"unless"`
}

type FilteringConfig struct {
	CheckExtern  bool              `toml:"check_extern"`
	Ignore       []string          `toml:"ignore"`
	IgnoreUnless []IgnoreUnlessRule `toml:"ignore_unless"`
	Nofollow     []string          `toml:"nofollow"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{
		Checking: CheckingConfig{
			SSLVerify: true,
			Threads:   10,
			MaxDepth:  0,
			Timeout:   "20s",
		},
	}

	if path == "" {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *CheckingConfig) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return 20 * time.Second
	}
	return d
}
