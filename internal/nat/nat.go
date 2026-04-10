package nat

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/google/nftables"
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
	elems  ElementRunner
	refs   map[string]natEntry
}

type natEntry struct {
	internalIP string
	refCount   int
}

type ElementRunner interface {
	Add(ctx context.Context, realIP, internalIP string) error
	Delete(ctx context.Context, realIP string) error
}

type execElementRunner struct {
	runner ScriptRunner
}

func (r execElementRunner) Add(ctx context.Context, realIP, internalIP string) error {
	script := fmt.Sprintf("add element ip mcproxy_nat nat_map { %s : %s }\n", realIP, internalIP)
	return r.runner.Run(ctx, script)
}

func (r execElementRunner) Delete(ctx context.Context, realIP string) error {
	script := fmt.Sprintf("delete element ip mcproxy_nat nat_map { %s }\n", realIP)
	return r.runner.Run(ctx, script)
}

type nativeElementRunner struct {
	mu   sync.Mutex
	conn *nftables.Conn
	set  *nftables.Set
}

func newNativeElementRunner() *nativeElementRunner {
	return &nativeElementRunner{
		set: &nftables.Set{
			Table: &nftables.Table{
				Family: nftables.TableFamilyIPv4,
				Name:   "mcproxy_nat",
			},
			Name: "nat_map",
		},
	}
}

func (r *nativeElementRunner) Add(ctx context.Context, realIP, internalIP string) error {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()

	realIPv4 := net.ParseIP(realIP).To4()
	internalIPv4 := net.ParseIP(internalIP).To4()
	if realIPv4 == nil || internalIPv4 == nil {
		return fmt.Errorf("nat map requires ipv4 addresses, got real_ip=%q internal_ip=%q", realIP, internalIP)
	}

	conn, err := r.connection()
	if err != nil {
		return err
	}

	if err := conn.SetAddElements(r.set, []nftables.SetElement{{
		Key: []byte(realIPv4),
		Val: []byte(internalIPv4),
	}}); err != nil {
		return ignoreAlreadyExists(fmt.Errorf("native nft add element failed: %w", err))
	}
	if err := conn.Flush(); err != nil {
		return ignoreAlreadyExists(fmt.Errorf("native nft flush add failed: %w", err))
	}
	return nil
}

func (r *nativeElementRunner) Delete(ctx context.Context, realIP string) error {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()

	realIPv4 := net.ParseIP(realIP).To4()
	if realIPv4 == nil {
		return fmt.Errorf("nat map requires ipv4 real_ip, got %q", realIP)
	}

	conn, err := r.connection()
	if err != nil {
		return err
	}

	if err := conn.SetDeleteElements(r.set, []nftables.SetElement{{
		Key: []byte(realIPv4),
	}}); err != nil {
		return ignoreMissing(fmt.Errorf("native nft delete element failed: %w", err))
	}
	if err := conn.Flush(); err != nil {
		return ignoreMissing(fmt.Errorf("native nft flush delete failed: %w", err))
	}
	return nil
}

func (r *nativeElementRunner) connection() (*nftables.Conn, error) {
	if r.conn != nil {
		return r.conn, nil
	}
	conn, err := nftables.New(nftables.AsLasting())
	if err != nil {
		return nil, fmt.Errorf("open nftables conn: %w", err)
	}
	r.conn = conn
	return conn, nil
}

func NewNATManager(runner ScriptRunner) *NATManager {
	return &NATManager{
		runner: runner,
		elems:  execElementRunner{runner: runner},
		refs:   make(map[string]natEntry),
	}
}

func NewNativeNATManager(runner ScriptRunner) *NATManager {
	return &NATManager{
		runner: runner,
		elems:  newNativeElementRunner(),
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

	if err := m.elems.Add(ctx, realIP, internalIP); err != nil {
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

	if err := ignoreMissing(m.elems.Delete(ctx, realIP)); err != nil {
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

type DNATDestination struct {
	IP   string
	Port int
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

func (m *NodeDNATManager) EnsureDestinations(ctx context.Context, tailscaleInterface string, destinations map[int]DNATDestination) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ports := make([]int, 0, len(destinations))
	for port := range destinations {
		ports = append(ports, port)
	}
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
		dst := destinations[port]
		rule := fmt.Sprintf("add rule ip mcproxy_node prerouting iifname %q tcp dport %d dnat to %s:%d\n", tailscaleInterface, port, dst.IP, dst.Port)
		if err := m.runner.Run(ctx, rule); err != nil {
			return err
		}
	}
	return nil
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
