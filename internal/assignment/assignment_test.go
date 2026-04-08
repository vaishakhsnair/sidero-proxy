package assignment

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/registration"
)

func TestAssignReusesExistingMapping(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	assigner := New(rdb, "proxy-1", 1, 3600)
	first, err := assigner.Assign(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("Assign() first error = %v", err)
	}
	second, err := assigner.Assign(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("Assign() second error = %v", err)
	}
	if first != second {
		t.Fatalf("Assign() reuse mismatch: first=%s second=%s", first, second)
	}
	if first != "10.1.0.1" {
		t.Fatalf("Assign() = %s, want 10.1.0.1", first)
	}
}

func TestReserveForAllProxiesCreatesPermanentMappings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	assigner := New(rdb, "proxy-1", 1, 60)
	if _, err := assigner.Assign(ctx, "1.2.3.4"); err != nil {
		t.Fatalf("Assign() error = %v", err)
	}

	proxies := map[string]registration.ProxyInfo{
		"proxy-1": {Subnet: "10.1.0.0/16"},
		"proxy-2": {Subnet: "10.2.0.0/16"},
	}

	got, err := assigner.ReserveForAllProxies(ctx, "1.2.3.4", proxies)
	if err != nil {
		t.Fatalf("ReserveForAllProxies() error = %v", err)
	}
	if got["proxy-1"] != "10.1.0.1" {
		t.Fatalf("proxy-1 mapping = %s, want 10.1.0.1", got["proxy-1"])
	}
	if got["proxy-2"] != "10.2.0.1" {
		t.Fatalf("proxy-2 mapping = %s, want 10.2.0.1", got["proxy-2"])
	}

	mr.FastForward(2 * time.Minute)

	all, err := assigner.LookupMappings(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("LookupMappings() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("LookupMappings() len = %d, want 2", len(all))
	}

	reverse, err := assigner.LookupReverse(ctx, "10.2.0.1")
	if err != nil {
		t.Fatalf("LookupReverse() error = %v", err)
	}
	if reverse.RealIP != "1.2.3.4" || reverse.Proxy != "proxy-2" {
		t.Fatalf("LookupReverse() = %+v, want real_ip 1.2.3.4 proxy-2", reverse)
	}

	reserved, err := rdb.SIsMember(ctx, "reserved:real", "1.2.3.4").Result()
	if err != nil {
		t.Fatalf("SIsMember() error = %v", err)
	}
	if !reserved {
		t.Fatal("reserved:real missing promoted real IP")
	}
}

func TestLookupReverseRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mr.Set("rmap:10.1.0.1", "nope")
	assigner := New(rdb, "", 0, 0)
	if _, err := assigner.LookupReverse(ctx, "10.1.0.1"); err == nil {
		t.Fatal("LookupReverse() error = nil, want error")
	}
}

func TestCounterToInternalIP(t *testing.T) {
	t.Parallel()

	got, err := CounterToInternalIP(7, 513)
	if err != nil {
		t.Fatalf("CounterToInternalIP() error = %v", err)
	}
	if got != "10.7.2.1" {
		t.Fatalf("CounterToInternalIP() = %s, want 10.7.2.1", got)
	}
}

func TestLookupReverseJSONShape(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(ReverseMapping{RealIP: "1.1.1.1", Proxy: "proxy-1"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(data) != `{"real_ip":"1.1.1.1","proxy":"proxy-1"}` {
		t.Fatalf("Marshal() = %s", string(data))
	}
}
