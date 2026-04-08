package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Server struct {
	Name          string `json:"name"`
	ProxyPublicIP string `json:"proxy_public_ip"`
	BackendIP     string `json:"backend_ip"`
}

type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type ProxyConfig struct {
	ProxyID       string    `json:"proxy_id"`
	RedisAddr     string    `json:"redis_addr"`
	RedisPassword string    `json:"redis_password"`
	IPTTLSeconds  int       `json:"ip_ttl_seconds"`
	InterceptPort int       `json:"intercept_port"`
	PortRange     PortRange `json:"port_range"`
	Servers       []Server  `json:"servers"`
}

type NodeDNATConfig struct {
	PublicIP           string    `json:"public_ip"`
	TailscaleInterface string    `json:"tailscale_interface"`
	PortRange          PortRange `json:"port_range"`
}

type WatcherConfig struct {
	RedisAddr     string         `json:"redis_addr"`
	RedisPassword string         `json:"redis_password"`
	VolumesRoot   string         `json:"volumes_root"`
	NodeDNAT      NodeDNATConfig `json:"node_dnat"`
}

func LoadProxy(path string) (*ProxyConfig, error) {
	var cfg ProxyConfig
	if err := loadJSON(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func LoadWatcher(path string) (*WatcherConfig, error) {
	var cfg WatcherConfig
	if err := loadJSON(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *ProxyConfig) Validate() error {
	if strings.TrimSpace(c.ProxyID) == "" {
		return fmt.Errorf("proxy_id is required")
	}
	if strings.TrimSpace(c.RedisAddr) == "" {
		return fmt.Errorf("redis_addr is required")
	}
	if len(c.Servers) == 0 {
		return fmt.Errorf("at least one server is required")
	}
	if c.InterceptPort <= 0 || c.InterceptPort > 65535 {
		return fmt.Errorf("intercept_port must be between 1 and 65535")
	}
	if err := c.PortRange.Validate("port_range"); err != nil {
		return err
	}
	for i, server := range c.Servers {
		if strings.TrimSpace(server.ProxyPublicIP) == "" {
			return fmt.Errorf("servers[%d].proxy_public_ip is required", i)
		}
		if strings.TrimSpace(server.BackendIP) == "" {
			return fmt.Errorf("servers[%d].backend_ip is required", i)
		}
	}
	if c.IPTTLSeconds < 0 {
		return fmt.Errorf("ip_ttl_seconds cannot be negative")
	}
	return nil
}

func (c *WatcherConfig) Validate() error {
	if strings.TrimSpace(c.RedisAddr) == "" {
		return fmt.Errorf("redis_addr is required")
	}
	if strings.TrimSpace(c.VolumesRoot) == "" {
		return fmt.Errorf("volumes_root is required")
	}
	if c.NodeDNAT.PublicIP != "" || c.NodeDNAT.TailscaleInterface != "" || c.NodeDNAT.PortRange.Start != 0 || c.NodeDNAT.PortRange.End != 0 {
		if strings.TrimSpace(c.NodeDNAT.PublicIP) == "" {
			return fmt.Errorf("node_dnat.public_ip is required when node_dnat is configured")
		}
		if strings.TrimSpace(c.NodeDNAT.TailscaleInterface) == "" {
			return fmt.Errorf("node_dnat.tailscale_interface is required when node_dnat is configured")
		}
		if err := c.NodeDNAT.PortRange.Validate("node_dnat.port_range"); err != nil {
			return err
		}
	}
	return nil
}

func (p PortRange) Validate(field string) error {
	if p.Start <= 0 || p.Start > 65535 {
		return fmt.Errorf("%s.start must be between 1 and 65535", field)
	}
	if p.End <= 0 || p.End > 65535 {
		return fmt.Errorf("%s.end must be between 1 and 65535", field)
	}
	if p.End < p.Start {
		return fmt.Errorf("%s.end must be greater than or equal to %s.start", field, field)
	}
	return nil
}

func (p PortRange) Ports() []int {
	ports := make([]int, 0, p.End-p.Start+1)
	for port := p.Start; port <= p.End; port++ {
		ports = append(ports, port)
	}
	return ports
}

func loadJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
