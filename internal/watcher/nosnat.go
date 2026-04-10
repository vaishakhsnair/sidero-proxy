package watcher

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

const proxySubnetPoolCIDR = "10.0.0.0/8"

type iptablesRunner interface {
	Run(ctx context.Context, args ...string) error
}

type ExecIPTablesRunner struct{}

func (ExecIPTablesRunner) Run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "iptables", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("iptables %v failed: %w: %s", args, err, msg)
		}
		return fmt.Errorf("iptables %v failed: %w", args, err)
	}
	return nil
}

type NoSNATManager struct {
	mu     sync.Mutex
	runner iptablesRunner
}

func NewNoSNATManager(runner iptablesRunner) *NoSNATManager {
	return &NoSNATManager{runner: runner}
}

func (m *NoSNATManager) Ensure(ctx context.Context, bridgeSubnets []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sorted := append([]string(nil), bridgeSubnets...)
	sort.Strings(sorted)

	if err := ignoreIPTablesExists(m.runner.Run(ctx, "-t", "nat", "-N", "MCPROXY_NOSNAT")); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "-t", "nat", "-C", "POSTROUTING", "-j", "MCPROXY_NOSNAT"); err != nil {
		if err := m.runner.Run(ctx, "-t", "nat", "-I", "POSTROUTING", "1", "-j", "MCPROXY_NOSNAT"); err != nil {
			return err
		}
	}
	if err := m.runner.Run(ctx, "-t", "nat", "-F", "MCPROXY_NOSNAT"); err != nil {
		return err
	}
	for _, subnet := range sorted {
		if err := m.runner.Run(ctx, "-t", "nat", "-A", "MCPROXY_NOSNAT", "-s", proxySubnetPoolCIDR, "-d", subnet, "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return nil
}

func ignoreIPTablesExists(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "Chain already exists") {
		return nil
	}
	return err
}
