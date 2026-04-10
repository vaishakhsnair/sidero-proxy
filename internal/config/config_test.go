package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProxy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{
		"proxy_id":"proxy-1",
		"redis_addr":"127.0.0.1:6379",
		"ip_ttl_seconds":600,
		"intercept_port":19000,
		"health_port":18080,
		"port_range":{"start":25565,"end":25570},
		"servers":[{"name":"node-a","proxy_public_ip":"203.0.113.10","hostnames":["ingress-1.sidero.net"],"backend_ip":"100.64.0.10"}]
	}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadProxy(path)
	if err != nil {
		t.Fatalf("LoadProxy() error = %v", err)
	}
	if cfg.ProxyID != "proxy-1" {
		t.Fatalf("ProxyID = %q, want proxy-1", cfg.ProxyID)
	}
	if len(cfg.Servers) != 1 || cfg.PortRange.Start != 25565 || cfg.PortRange.End != 25570 || cfg.InterceptPort != 19000 || cfg.HealthPort != 18080 {
		t.Fatalf("Config = %+v, want one server and configured port range", cfg)
	}
	if len(cfg.Servers[0].Hostnames) != 1 || cfg.Servers[0].Hostnames[0] != "ingress-1.sidero.net" {
		t.Fatalf("Hostnames = %#v, want ingress hostname", cfg.Servers[0].Hostnames)
	}
}

func TestLoadWatcherValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "watcher.json")
	data := []byte(`{
		"redis_addr":"127.0.0.1:6379",
		"volumes_root":"/tmp/volumes",
		"node_dnat":{"public_ip":"167.235.15.102","tailscale_interface":"tailscale0","port_range":{"start":25565,"end":25570}}
	}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write watcher config: %v", err)
	}

	cfg, err := LoadWatcher(path)
	if err != nil {
		t.Fatalf("LoadWatcher() error = %v", err)
	}
	if cfg.NodeDNAT.DockerNetwork != "bridge" {
		t.Fatalf("DockerNetwork = %q, want bridge", cfg.NodeDNAT.DockerNetwork)
	}
}

func TestLoadFailover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "failover.json")
	data := []byte(`{
		"cloudflare":{"api_token":"token","zone_id":"zone"},
		"fallback_region":"sg",
		"probe_interval_seconds":10,
		"failback_window_seconds":60,
		"regions":[
			{"name":"us","health_url":"http://us-proxy:18080/healthz","answer_ips":["203.0.113.10"]},
			{"name":"sg","health_url":"http://sg-proxy:18080/healthz","answer_ips":["203.0.113.20"]}
		],
		"hostnames":[
			{"name":"server1.example.com","region":"us","ttl":60}
		]
	}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write failover config: %v", err)
	}

	cfg, err := LoadFailover(path)
	if err != nil {
		t.Fatalf("LoadFailover() error = %v", err)
	}
	if cfg.FallbackRegion != "sg" || len(cfg.Regions) != 2 || len(cfg.Hostnames) != 1 {
		t.Fatalf("Config = %+v, want parsed failover config", cfg)
	}
}
