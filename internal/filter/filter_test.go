package filter

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	scripts []string
}

func (f *fakeRunner) Run(_ context.Context, script string) error {
	f.scripts = append(f.scripts, script)
	return nil
}

func TestEnsureScript(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewManager(runner)
	if err := manager.Ensure(context.Background(), []string{"203.0.113.11", "203.0.113.10"}, 25565, 25566); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	got := strings.Join(runner.scripts, "")
	if !strings.Contains(got, "flush set ip mcproxy_filter public_ips") {
		t.Fatalf("missing public_ips flush: %q", got)
	}
	if !strings.Contains(got, "203.0.113.10, 203.0.113.11") {
		t.Fatalf("public IPs not sorted: %q", got)
	}
	if !strings.Contains(got, "ip saddr @blocklist drop") || !strings.Contains(got, "limit rate 50/second burst 100 packets accept") {
		t.Fatalf("missing filter rules: %q", got)
	}
}

func TestBlockAndUnblock(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewManager(runner)
	if err := manager.Block(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if err := manager.Unblock(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("Unblock() error = %v", err)
	}
	got := strings.Join(runner.scripts, "")
	if !strings.Contains(got, "add element ip mcproxy_filter blocklist { 1.2.3.4 }") || !strings.Contains(got, "delete element ip mcproxy_filter blocklist { 1.2.3.4 }") {
		t.Fatalf("block/unblock scripts = %q", got)
	}
}
