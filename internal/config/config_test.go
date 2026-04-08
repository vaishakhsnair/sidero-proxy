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
		"port_range":{"start":25565,"end":25570},
		"servers":[{"name":"node-a","proxy_public_ip":"203.0.113.10","backend_ip":"100.64.0.10"}]
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
	if len(cfg.Servers) != 1 || cfg.PortRange.Start != 25565 || cfg.PortRange.End != 25570 || cfg.InterceptPort != 19000 {
		t.Fatalf("Config = %+v, want one server and configured port range", cfg)
	}
}

func TestLoadWatcherValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "watcher.json")
	data := []byte(`{
		"redis_addr":"127.0.0.1:6379",
		"volumes_root":"/tmp/volumes",
		"node_dnat":{"public_ip":"167.235.15.102","port_range":{"start":25565,"end":25570}}
	}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write watcher config: %v", err)
	}

	if _, err := LoadWatcher(path); err == nil {
		t.Fatal("LoadWatcher() error = nil, want validation error")
	}
}
