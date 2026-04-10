package nat

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	scripts []string
	err     error
}

func (f *fakeRunner) Run(_ context.Context, script string) error {
	f.scripts = append(f.scripts, script)
	return f.err
}

func TestNATManagerEnsureScript(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewNATManager(runner)
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if len(runner.scripts) != 5 {
		t.Fatalf("scripts = %d, want 5", len(runner.scripts))
	}
	if runner.scripts[3] != "flush chain ip mcproxy_nat postrouting\n" {
		t.Fatalf("flush script = %q", runner.scripts[3])
	}
	if !strings.Contains(runner.scripts[4], "snat to ip saddr map @nat_map") {
		t.Fatalf("rule script = %q", runner.scripts[4])
	}
}

func TestNATManagerRefCountsSameRealIP(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewNATManager(runner)
	if err := manager.Add(context.Background(), "1.2.3.4", "10.1.0.1"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := manager.Add(context.Background(), "1.2.3.4", "10.1.0.1"); err != nil {
		t.Fatalf("second Add() error = %v", err)
	}
	if len(runner.scripts) != 1 {
		t.Fatalf("scripts after duplicate add = %d, want 1", len(runner.scripts))
	}
	if err := manager.Delete(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("first Delete() error = %v", err)
	}
	if len(runner.scripts) != 1 {
		t.Fatalf("scripts after first delete = %d, want 1", len(runner.scripts))
	}
	if err := manager.Delete(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}
	if len(runner.scripts) != 2 {
		t.Fatalf("scripts after final delete = %d, want 2", len(runner.scripts))
	}
	if !strings.Contains(runner.scripts[1], "delete element ip mcproxy_nat nat_map { 1.2.3.4 }") {
		t.Fatalf("final delete script = %q", runner.scripts[1])
	}
}

func TestNATManagerRejectsConflictingInternalIP(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewNATManager(runner)
	if err := manager.Add(context.Background(), "1.2.3.4", "10.1.0.1"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := manager.Add(context.Background(), "1.2.3.4", "10.1.0.2"); err == nil {
		t.Fatal("second Add() error = nil, want conflict error")
	}
}

func TestNodeDNATEnsureScript(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewNodeDNATManager(runner)
	if err := manager.Ensure(context.Background(), "167.235.15.102", "tailscale0", []int{25565}); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if len(runner.scripts) != 4 {
		t.Fatalf("scripts = %d, want 4", len(runner.scripts))
	}
	if runner.scripts[2] != "flush chain ip mcproxy_node prerouting\n" {
		t.Fatalf("Ensure() flush = %q", runner.scripts[2])
	}
	if runner.scripts[3] != "add rule ip mcproxy_node prerouting iifname \"tailscale0\" tcp dport 25565 dnat to 167.235.15.102:25565\n" {
		t.Fatalf("Ensure() rule = %q", runner.scripts[3])
	}
}

func TestNodeDNATEnsureRangeScript(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewNodeDNATManager(runner)
	if err := manager.EnsureRange(context.Background(), "167.235.15.102", "tailscale0", 25565, 25566); err != nil {
		t.Fatalf("EnsureRange() error = %v", err)
	}
	if len(runner.scripts) != 5 {
		t.Fatalf("scripts = %d, want 5", len(runner.scripts))
	}
	got := strings.Join(runner.scripts, "")
	if !strings.Contains(got, `tcp dport 25565`) || !strings.Contains(got, `tcp dport 25566`) {
		t.Fatalf("EnsureRange() script = %q", got)
	}
}

func TestNodeDNATEnsureDestinationsScript(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewNodeDNATManager(runner)
	destinations := map[int]DNATDestination{
		25566: {IP: "172.18.0.12", Port: 25565},
		25565: {IP: "172.18.0.11", Port: 25565},
	}
	if err := manager.EnsureDestinations(context.Background(), "tailscale0", destinations); err != nil {
		t.Fatalf("EnsureDestinations() error = %v", err)
	}
	if len(runner.scripts) != 5 {
		t.Fatalf("scripts = %d, want 5", len(runner.scripts))
	}
	got := strings.Join(runner.scripts, "")
	if !strings.Contains(got, `tcp dport 25565 dnat to 172.18.0.11:25565`) || !strings.Contains(got, `tcp dport 25566 dnat to 172.18.0.12:25565`) {
		t.Fatalf("EnsureDestinations() script = %q", got)
	}
}

func TestProxyRedirectEnsureScript(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewProxyRedirectManager(runner)
	if err := manager.Ensure(context.Background(), []string{"203.0.113.11", "203.0.113.10"}, 25565, 25570, 19000); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if len(runner.scripts) != 7 {
		t.Fatalf("scripts = %d, want 7", len(runner.scripts))
	}
	got := strings.Join(runner.scripts, "")
	if strings.Contains(got, "delete table ip mcproxy_intercept") {
		t.Fatalf("Ensure() should not delete table: %q", got)
	}
	if !strings.Contains(got, "flush set ip mcproxy_intercept public_ips") || !strings.Contains(got, "flush chain ip mcproxy_intercept prerouting") || !strings.Contains(got, "redirect to :19000") {
		t.Fatalf("Ensure() script = %q", got)
	}
	if !strings.Contains(got, "203.0.113.10, 203.0.113.11") {
		t.Fatalf("Ensure() script did not sort public IPs: %q", got)
	}
}
