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
	"sidero-proxy/internal/nat"
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

	service := New(&config.WatcherConfig{VolumesRoot: "/tmp"}, rdb, assigner, nil, nil, slog.Default())
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

	service := New(&config.WatcherConfig{VolumesRoot: dir}, rdb, assigner, nil, nil, slog.Default())
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

type fakeResolver struct {
	destinations map[int]nat.DNATDestination
	err          error
}

func (f fakeResolver) Resolve(_ context.Context) (map[int]nat.DNATDestination, error) {
	return f.destinations, f.err
}

func (f fakeResolver) Events(_ context.Context) (<-chan struct{}, <-chan error) {
	triggerCh := make(chan struct{})
	errCh := make(chan error)
	close(triggerCh)
	close(errCh)
	return triggerCh, errCh
}

type fakeDNAT struct {
	tailscaleInterface string
	destinations       map[int]nat.DNATDestination
}

func (f *fakeDNAT) EnsureDestinations(_ context.Context, tailscaleInterface string, destinations map[int]nat.DNATDestination) error {
	f.tailscaleInterface = tailscaleInterface
	f.destinations = destinations
	return nil
}

func (f *fakeDNAT) EnsureRange(_ context.Context, _ string, _ string, _, _ int) error {
	return nil
}

func TestEnsureNodeDNATUsesResolvedContainerEndpoints(t *testing.T) {
	t.Parallel()

	service := New(&config.WatcherConfig{
		VolumesRoot: "/tmp",
		NodeDNAT: config.NodeDNATConfig{
			PublicIP:           "167.235.15.102",
			TailscaleInterface: "tailscale0",
			DockerNetwork:      "bridge",
			PortRange:          config.PortRange{Start: 25551, End: 25551},
		},
	}, nil, nil, &fakeDNAT{}, nil, slog.Default())

	dnat := &fakeDNAT{}
	service.dnat = dnat
	service.resolver = fakeResolver{
		destinations: map[int]nat.DNATDestination{
			25551: {IP: "172.18.0.23", Port: 25551},
		},
	}
	if err := service.ensureNodeDNAT(context.Background()); err != nil {
		t.Fatalf("ensureNodeDNATWithManager() error = %v", err)
	}
	if dnat.tailscaleInterface != "tailscale0" {
		t.Fatalf("tailscale interface = %q, want tailscale0", dnat.tailscaleInterface)
	}
	got := dnat.destinations[25551]
	if got.IP != "172.18.0.23" || got.Port != 25551 {
		t.Fatalf("destination = %+v, want 172.18.0.23:25551", got)
	}
}
