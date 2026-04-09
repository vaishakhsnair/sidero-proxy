package nat

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

type NATManager struct {
	mu     sync.Mutex
	runner ScriptRunner
	refs   map[string]natEntry
}

type natEntry struct {
	internalIP string
	refCount   int
}

func NewNATManager(runner ScriptRunner) *NATManager {
	return &NATManager{
		runner: runner,
		refs:   make(map[string]natEntry),
	}
}

func (m *NATManager) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add table ip mcproxy_nat\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add chain ip mcproxy_nat postrouting { type nat hook postrouting priority srcnat; }\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add map ip mcproxy_nat nat_map { type ipv4_addr : ipv4_addr; }\n")); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "flush chain ip mcproxy_nat postrouting\n"); err != nil {
		return err
	}
	return m.runner.Run(ctx, "add rule ip mcproxy_nat postrouting snat to ip saddr map @nat_map\n")
}

func (m *NATManager) Add(ctx context.Context, realIP, internalIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.refs[realIP]
	if ok {
		if entry.internalIP != internalIP {
			return fmt.Errorf("nat entry for %s already exists with internal ip %s, cannot replace with %s while active", realIP, entry.internalIP, internalIP)
		}
		entry.refCount++
		m.refs[realIP] = entry
		return nil
	}

	script := fmt.Sprintf("add element ip mcproxy_nat nat_map { %s : %s }\n", realIP, internalIP)
	if err := m.runner.Run(ctx, script); err != nil {
		return err
	}
	m.refs[realIP] = natEntry{
		internalIP: internalIP,
		refCount:   1,
	}
	return nil
}

func (m *NATManager) Delete(ctx context.Context, realIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.refs[realIP]
	if !ok {
		return nil
	}
	if entry.refCount > 1 {
		entry.refCount--
		m.refs[realIP] = entry
		return nil
	}

	script := fmt.Sprintf("delete element ip mcproxy_nat nat_map { %s }\n", realIP)
	if err := ignoreMissing(m.runner.Run(ctx, script)); err != nil {
		return err
	}
	delete(m.refs, realIP)
	return nil
}

type ProxyRedirectManager struct {
	mu     sync.Mutex
	runner ScriptRunner
}

func NewProxyRedirectManager(runner ScriptRunner) *ProxyRedirectManager {
	return &ProxyRedirectManager{runner: runner}
}

func (m *ProxyRedirectManager) Ensure(ctx context.Context, publicIPs []string, startPort, endPort, interceptPort int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sortedIPs := append([]string(nil), publicIPs...)
	sort.Strings(sortedIPs)

	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add table ip mcproxy_intercept\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add set ip mcproxy_intercept public_ips { type ipv4_addr; }\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add chain ip mcproxy_intercept prerouting { type nat hook prerouting priority dstnat; }\n")); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "flush set ip mcproxy_intercept public_ips\n"); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "flush chain ip mcproxy_intercept prerouting\n"); err != nil {
		return err
	}

	if len(sortedIPs) > 0 {
		var b strings.Builder
		b.WriteString("add element ip mcproxy_intercept public_ips { ")
		for i, ip := range sortedIPs {
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

	rule := fmt.Sprintf("add rule ip mcproxy_intercept prerouting ip daddr @public_ips tcp dport %d-%d redirect to :%d\n", startPort, endPort, interceptPort)
	return m.runner.Run(ctx, rule)
}

type NodeDNATManager struct {
	mu     sync.Mutex
	runner ScriptRunner
}

func NewNodeDNATManager(runner ScriptRunner) *NodeDNATManager {
	return &NodeDNATManager{runner: runner}
}

func (m *NodeDNATManager) Ensure(ctx context.Context, publicIP, tailscaleInterface string, ports []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sort.Ints(ports)
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add table ip mcproxy_node\n")); err != nil {
		return err
	}
	if err := ignoreAlreadyExists(m.runner.Run(ctx, "add chain ip mcproxy_node prerouting { type nat hook prerouting priority dstnat; }\n")); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "flush chain ip mcproxy_node prerouting\n"); err != nil {
		return err
	}
	for _, port := range ports {
		rule := fmt.Sprintf("add rule ip mcproxy_node prerouting iifname %q tcp dport %d dnat to %s:%d\n", tailscaleInterface, port, publicIP, port)
		if err := m.runner.Run(ctx, rule); err != nil {
			return err
		}
	}
	return nil
}

func (m *NodeDNATManager) EnsureRange(ctx context.Context, publicIP, tailscaleInterface string, startPort, endPort int) error {
	ports := make([]int, 0, endPort-startPort+1)
	for port := startPort; port <= endPort; port++ {
		ports = append(ports, port)
	}
	return m.Ensure(ctx, publicIP, tailscaleInterface, ports)
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
