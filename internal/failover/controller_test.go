package failover

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"sidero-proxy/internal/config"
)

type fakeHealthChecker struct {
	healthy map[string]bool
}

func (f fakeHealthChecker) Healthy(_ context.Context, url string) error {
	if f.healthy[url] {
		return nil
	}
	return context.DeadlineExceeded
}

type fakeDNSClient struct {
	records map[string][]DNSRecord
}

func (f *fakeDNSClient) ListARecords(_ context.Context, _ string, name string) ([]DNSRecord, error) {
	return append([]DNSRecord(nil), f.records[name]...), nil
}

func (f *fakeDNSClient) DeleteRecord(_ context.Context, _ string, recordID string) error {
	for name, records := range f.records {
		filtered := records[:0]
		for _, record := range records {
			if record.ID != recordID {
				filtered = append(filtered, record)
			}
		}
		f.records[name] = filtered
	}
	return nil
}

func (f *fakeDNSClient) CreateARecord(_ context.Context, _ string, name, content string, ttl int) error {
	f.records[name] = append(f.records[name], DNSRecord{ID: name + "-" + content, Name: name, Content: content, TTL: ttl, Type: "A"})
	return nil
}

func TestControllerFallsBackAndFailsBack(t *testing.T) {
	t.Parallel()

	cfg := &config.FailoverConfig{
		Cloudflare:            config.FailoverCloudflare{APIToken: "token", ZoneID: "zone"},
		FallbackRegion:        "sg",
		ProbeIntervalSeconds:  5,
		FailbackWindowSeconds: 60,
		Regions: []config.FailoverRegion{
			{Name: "us", HealthURL: "http://us/healthz", AnswerIPs: []string{"203.0.113.10"}},
			{Name: "sg", HealthURL: "http://sg/healthz", AnswerIPs: []string{"203.0.113.20"}},
		},
		Hostnames: []config.FailoverHostname{
			{Name: "server.example.com", Region: "us", TTL: 60},
		},
	}
	dns := &fakeDNSClient{records: map[string][]DNSRecord{
		"server.example.com": {{ID: "a1", Name: "server.example.com", Content: "203.0.113.10", TTL: 60, Type: "A"}},
	}}
	checker := fakeHealthChecker{healthy: map[string]bool{
		"http://us/healthz": false,
		"http://sg/healthz": true,
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller := NewController(cfg, checker, dns, logger)
	base := time.Unix(100, 0)
	controller.now = func() time.Time { return base }

	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := dns.records["server.example.com"][0].Content; got != "203.0.113.20" {
		t.Fatalf("fallback record = %q, want sg ip", got)
	}

	controller.checker = fakeHealthChecker{healthy: map[string]bool{
		"http://us/healthz": true,
		"http://sg/healthz": true,
	}}
	controller.now = func() time.Time { return base.Add(30 * time.Second) }
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := dns.records["server.example.com"][0].Content; got != "203.0.113.20" {
		t.Fatalf("record before failback window = %q, want sg ip", got)
	}

	controller.now = func() time.Time { return base.Add(91 * time.Second) }
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := dns.records["server.example.com"][0].Content; got != "203.0.113.10" {
		t.Fatalf("record after failback window = %q, want us ip", got)
	}
}
