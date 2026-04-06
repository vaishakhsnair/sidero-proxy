package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type NodeConfig struct {
	PublicIP    string `json:"public_ip"`
	TailscaleIP string `json:"tailscale_ip"`
	Name        string `json:"name"`
}

type Config struct {
	ProxyID       string       `json:"proxy_id"`
	RedisAddr     string       `json:"redis_addr"`
	RedisPassword string       `json:"redis_password"`
	IPTTLSeconds  int          `json:"ip_ttl_seconds"`
	TProxyPort    int          `json:"tproxy_port"`
	Nodes         []NodeConfig `json:"nodes"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.ProxyID == "" {
		return nil, fmt.Errorf("proxy_id is required")
	}
	if cfg.TProxyPort == 0 {
		cfg.TProxyPort = 8080
	}
	if cfg.IPTTLSeconds == 0 {
		cfg.IPTTLSeconds = 604800
	}
	return &cfg, nil
}
