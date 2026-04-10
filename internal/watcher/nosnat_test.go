package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeIPTablesRunner struct {
	commands [][]string
	failC    bool
}

func (f *fakeIPTablesRunner) Run(_ context.Context, args ...string) error {
	f.commands = append(f.commands, append([]string(nil), args...))
	if f.failC && len(args) >= 5 && args[0] == "-t" && args[1] == "nat" && args[2] == "-C" && args[3] == "POSTROUTING" {
		return fmt.Errorf("iptables check failed")
	}
	return nil
}

func TestNoSNATManagerEnsure(t *testing.T) {
	t.Parallel()

	runner := &fakeIPTablesRunner{failC: true}
	manager := NewNoSNATManager(runner)
	if err := manager.Ensure(context.Background(), []string{"172.18.0.0/16"}); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	got := flattenCommands(runner.commands)
	if !strings.Contains(got, "-t nat -I POSTROUTING 1 -j MCPROXY_NOSNAT") {
		t.Fatalf("missing POSTROUTING insert: %s", got)
	}
	if !strings.Contains(got, "-t nat -A MCPROXY_NOSNAT -s 10.0.0.0/8 -d 172.18.0.0/16 -j ACCEPT") {
		t.Fatalf("missing bridge subnet exemption: %s", got)
	}
}

func flattenCommands(commands [][]string) string {
	lines := make([]string, 0, len(commands))
	for _, cmd := range commands {
		lines = append(lines, strings.Join(cmd, " "))
	}
	return strings.Join(lines, "\n")
}
