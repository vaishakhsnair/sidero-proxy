package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Server struct {
	Name          string   `json:"name"`
	ProxyPublicIP string   `json:"proxy_public_ip"`
	Hostnames     []string `json:"hostnames"`
	BackendIP     string   `json:"backend_ip"`
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
	HealthPort    int       `json:"health_port"`
	PortRange     PortRange `json:"port_range"`
	Servers       []Server  `json:"servers"`
}

type NodeDNATConfig struct {
	PublicIP           string    `json:"public_ip"`
	TailscaleInterface string    `json:"tailscale_interface"`
	DockerNetwork      string    `json:"docker_network"`
	PortRange          PortRange `json:"port_range"`
}

type WatcherConfig struct {
	RedisAddr     string         `json:"redis_addr"`
	RedisPassword string         `json:"redis_password"`
	VolumesRoot   string         `json:"volumes_root"`
	NodeDNAT      NodeDNATConfig `json:"node_dnat"`
}

type FailoverRegion struct {
	Name      string   `json:"name"`
	HealthURL string   `json:"health_url"`
	AnswerIPs []string `json:"answer_ips"`
}

type FailoverHostname struct {
	Name   string `json:"name"`
	Region string `json:"region"`
	TTL    int    `json:"ttl"`
}

type FailoverCloudflare struct {
	APIToken string `json:"api_token"`
	ZoneID   string `json:"zone_id"`
}

type FailoverConfig struct {
	Cloudflare            FailoverCloudflare `json:"cloudflare"`
	FallbackRegion        string             `json:"fallback_region"`
	ProbeIntervalSeconds  int                `json:"probe_interval_seconds"`
	FailbackWindowSeconds int                `json:"failback_window_seconds"`
	Regions               []FailoverRegion   `json:"regions"`
	Hostnames             []FailoverHostname `json:"hostnames"`
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

func LoadFailover(path string) (*FailoverConfig, error) {
	var cfg FailoverConfig
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
	if c.HealthPort <= 0 || c.HealthPort > 65535 {
		return fmt.Errorf("health_port must be between 1 and 65535")
	}
	if c.HealthPort == c.InterceptPort {
		return fmt.Errorf("health_port must differ from intercept_port")
	}
	if err := c.PortRange.Validate("port_range"); err != nil {
		return err
	}
	hasIngressIP := false
	for i, server := range c.Servers {
		if strings.TrimSpace(server.ProxyPublicIP) != "" {
			hasIngressIP = true
		}
		if strings.TrimSpace(server.ProxyPublicIP) == "" && len(server.Hostnames) == 0 {
			return fmt.Errorf("servers[%d] must define proxy_public_ip or hostnames", i)
		}
		for j, hostname := range server.Hostnames {
			if strings.TrimSpace(hostname) == "" {
				return fmt.Errorf("servers[%d].hostnames[%d] must not be empty", i, j)
			}
		}
		if strings.TrimSpace(server.BackendIP) == "" {
			return fmt.Errorf("servers[%d].backend_ip is required", i)
		}
	}
	if !hasIngressIP {
		return fmt.Errorf("at least one servers[].proxy_public_ip is required")
	}
	if c.IPTTLSeconds < 0 {
		return fmt.Errorf("ip_ttl_seconds cannot be negative")
	}
	return nil
}

func (c *FailoverConfig) Validate() error {
	if strings.TrimSpace(c.Cloudflare.APIToken) == "" {
		return fmt.Errorf("cloudflare.api_token is required")
	}
	if strings.TrimSpace(c.Cloudflare.ZoneID) == "" {
		return fmt.Errorf("cloudflare.zone_id is required")
	}
	if strings.TrimSpace(c.FallbackRegion) == "" {
		return fmt.Errorf("fallback_region is required")
	}
	if c.ProbeIntervalSeconds <= 0 {
		return fmt.Errorf("probe_interval_seconds must be greater than zero")
	}
	if c.FailbackWindowSeconds < 0 {
		return fmt.Errorf("failback_window_seconds cannot be negative")
	}
	if len(c.Regions) == 0 {
		return fmt.Errorf("at least one region is required")
	}
	if len(c.Hostnames) == 0 {
		return fmt.Errorf("at least one hostname is required")
	}

	regionNames := make(map[string]struct{}, len(c.Regions))
	for i, region := range c.Regions {
		if strings.TrimSpace(region.Name) == "" {
			return fmt.Errorf("regions[%d].name is required", i)
		}
		if strings.TrimSpace(region.HealthURL) == "" {
			return fmt.Errorf("regions[%d].health_url is required", i)
		}
		if len(region.AnswerIPs) == 0 {
			return fmt.Errorf("regions[%d].answer_ips must not be empty", i)
		}
		regionNames[region.Name] = struct{}{}
	}
	if _, ok := regionNames[c.FallbackRegion]; !ok {
		return fmt.Errorf("fallback_region %q is not defined in regions", c.FallbackRegion)
	}
	for i, hostname := range c.Hostnames {
		if strings.TrimSpace(hostname.Name) == "" {
			return fmt.Errorf("hostnames[%d].name is required", i)
		}
		if strings.TrimSpace(hostname.Region) == "" {
			return fmt.Errorf("hostnames[%d].region is required", i)
		}
		if _, ok := regionNames[hostname.Region]; !ok {
			return fmt.Errorf("hostnames[%d].region %q is not defined in regions", i, hostname.Region)
		}
		if hostname.TTL < 1 {
			return fmt.Errorf("hostnames[%d].ttl must be greater than zero", i)
		}
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
		if strings.TrimSpace(c.NodeDNAT.DockerNetwork) == "" {
			c.NodeDNAT.DockerNetwork = "bridge"
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
