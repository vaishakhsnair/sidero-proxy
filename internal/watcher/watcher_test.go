package watcher

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/assignment"
	"sidero-proxy/internal/config"
	"sidero-proxy/internal/registration"
)

func TestPromoteIdentityReservesMappingsAcrossProxies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	assigner := assignment.New(rdb, "proxy-1", 1, 60)
	if _, err := assigner.Assign(ctx, "1.2.3.4"); err != nil {
		t.Fatalf("Assign() error = %v", err)
	}

	p1, _ := json.Marshal(registration.ProxyInfo{Subnet: "10.1.0.0/16"})
	p2, _ := json.Marshal(registration.ProxyInfo{Subnet: "10.2.0.0/16"})
	mr.HSet("proxy:registry", "proxy-1", string(p1))
	mr.HSet("proxy:registry", "proxy-2", string(p2))

	service := New(&config.WatcherConfig{VolumesRoot: "/tmp"}, rdb, assigner, nil, slog.Default())
	if err := service.promoteIdentity(ctx, BannedIPEntry{IP: "10.1.0.1"}); err != nil {
		t.Fatalf("promoteIdentity() error = %v", err)
	}

	mappings, err := assigner.LookupMappings(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("LookupMappings() error = %v", err)
	}
	if mappings["proxy-2"] != "10.2.0.1" {
		t.Fatalf("proxy-2 mapping = %q, want 10.2.0.1", mappings["proxy-2"])
	}
}

func TestHandleBannedIPsDetectsNewEntries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	assigner := assignment.New(rdb, "proxy-1", 1, 60)
	if _, err := assigner.Assign(ctx, "1.2.3.4"); err != nil {
		t.Fatalf("Assign() error = %v", err)
	}

	p1, _ := json.Marshal(registration.ProxyInfo{Subnet: "10.1.0.0/16"})
	p2, _ := json.Marshal(registration.ProxyInfo{Subnet: "10.2.0.0/16"})
	mr.HSet("proxy:registry", "proxy-1", string(p1))
	mr.HSet("proxy:registry", "proxy-2", string(p2))

	dir := t.TempDir()
	serverDir := filepath.Join(dir, "server-a")
	if err := os.Mkdir(serverDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	data := []byte(`[{"ip":"10.1.0.1","reason":"test"}]`)
	if err := os.WriteFile(filepath.Join(serverDir, "banned-ips.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	service := New(&config.WatcherConfig{VolumesRoot: dir}, rdb, assigner, nil, slog.Default())
	if err := service.handleBannedIPs(ctx, serverDir); err != nil {
		t.Fatalf("handleBannedIPs() error = %v", err)
	}

	mappings, err := assigner.LookupMappings(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("LookupMappings() error = %v", err)
	}
	if mappings["proxy-2"] != "10.2.0.1" {
		t.Fatalf("proxy-2 mapping = %q, want 10.2.0.1", mappings["proxy-2"])
	}
}
