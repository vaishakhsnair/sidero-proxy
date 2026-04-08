package filter

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

type ScriptRunner interface {
	Run(ctx context.Context, script string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type Manager struct {
	mu     sync.Mutex
	runner ScriptRunner
}

func NewManager(runner ScriptRunner) *Manager {
	return &Manager{runner: runner}
}

func (m *Manager) Ensure(ctx context.Context, publicIPs []string, startPort, endPort int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ips := append([]string(nil), publicIPs...)
	sort.Strings(ips)

	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add table ip mcproxy_filter\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add set ip mcproxy_filter public_ips { type ipv4_addr; }\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add set ip mcproxy_filter blocklist { type ipv4_addr; }\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add chain ip mcproxy_filter prerouting { type filter hook prerouting priority -150; }\n")); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "flush set ip mcproxy_filter public_ips\n"); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "flush chain ip mcproxy_filter prerouting\n"); err != nil {
		return err
	}
	if len(ips) > 0 {
		var b strings.Builder
		b.WriteString("add element ip mcproxy_filter public_ips { ")
		for i, ip := range ips {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ip)
		}
		b.WriteString(" }\n")
		if err := m.runner.Run(ctx, b.String()); err != nil {
			return err
		}
	}
	if err := m.runner.Run(ctx, fmt.Sprintf("add rule ip mcproxy_filter prerouting ip daddr @public_ips tcp dport %d-%d ip saddr @blocklist drop\n", startPort, endPort)); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, fmt.Sprintf("add rule ip mcproxy_filter prerouting ip daddr @public_ips tcp dport %d-%d tcp flags syn limit rate 50/second burst 100 packets accept\n", startPort, endPort)); err != nil {
		return err
	}
	return m.runner.Run(ctx, fmt.Sprintf("add rule ip mcproxy_filter prerouting ip daddr @public_ips tcp dport %d-%d tcp flags syn drop\n", startPort, endPort))
}

func (m *Manager) Block(ctx context.Context, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runner.Run(ctx, fmt.Sprintf("add element ip mcproxy_filter blocklist { %s }\n", ip))
}

func (m *Manager) Unblock(ctx context.Context, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ignoreMissing(m.runner.Run(ctx, fmt.Sprintf("delete element ip mcproxy_filter blocklist { %s }\n", ip)))
}

func ignoreAlreadyExists(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "File exists") {
		return nil
	}
	return err
}

func ignoreMissing(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "No such file") {
		return nil
	}
	return err
}
