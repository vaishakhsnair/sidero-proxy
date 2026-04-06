package nat

import (
	"fmt"
	"net"
	"sync"

	"github.com/google/nftables"
)

// NATManager manages the nftables nat_map (ip nat table) via netlink.
// The table and map must already exist (created by deploy/nftables-proxy.conf).
type NATManager struct {
	mu     sync.Mutex
	conn   *nftables.Conn
	tbl    *nftables.Table
	natMap *nftables.Set
}

func New() (*NATManager, error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("nftables.New: %w", err)
	}

	tables, err := conn.ListTables()
	if err != nil {
		return nil, fmt.Errorf("ListTables: %w", err)
	}

	var natTable *nftables.Table
	for _, t := range tables {
		if t.Name == "nat" && t.Family == nftables.TableFamilyIPv4 {
			natTable = t
			break
		}
	}
	if natTable == nil {
		return nil, fmt.Errorf("table ip nat not found — apply deploy/nftables-proxy.conf first")
	}

	sets, err := conn.GetSets(natTable)
	if err != nil {
		return nil, fmt.Errorf("GetSets: %w", err)
	}

	var natMap *nftables.Set
	for _, s := range sets {
		if s.Name == "nat_map" {
			natMap = s
			break
		}
	}
	if natMap == nil {
		return nil, fmt.Errorf("map nat_map not found — apply deploy/nftables-proxy.conf first")
	}

	return &NATManager{conn: conn, tbl: natTable, natMap: natMap}, nil
}

// Add inserts a realIP → internalIP entry into the nat_map.
func (n *NATManager) Add(realIP, internalIP string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	real := net.ParseIP(realIP).To4()
	internal := net.ParseIP(internalIP).To4()
	if real == nil || internal == nil {
		return fmt.Errorf("invalid IP: %q → %q", realIP, internalIP)
	}

	err := n.conn.SetAddElements(n.natMap, []nftables.SetElement{
		{Key: real, Val: internal},
	})
	if err != nil {
		return fmt.Errorf("SetAddElements: %w", err)
	}
	return n.conn.Flush()
}

// Delete removes a realIP entry from the nat_map.
func (n *NATManager) Delete(realIP string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	real := net.ParseIP(realIP).To4()
	if real == nil {
		return fmt.Errorf("invalid IP: %q", realIP)
	}

	err := n.conn.SetDeleteElements(n.natMap, []nftables.SetElement{
		{Key: real},
	})
	if err != nil {
		return fmt.Errorf("SetDeleteElements: %w", err)
	}
	return n.conn.Flush()
}

// Flush removes all elements from the nat_map.
func (n *NATManager) Flush() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.conn.FlushSet(n.natMap)
	return n.conn.Flush()
}

// BlocklistManager manages the inet filter blocklist set via netlink.
type BlocklistManager struct {
	mu        sync.Mutex
	conn      *nftables.Conn
	tbl       *nftables.Table
	blocklist *nftables.Set
}

func NewBlocklistManager() (*BlocklistManager, error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("nftables.New: %w", err)
	}

	tables, err := conn.ListTables()
	if err != nil {
		return nil, fmt.Errorf("ListTables: %w", err)
	}

	var filterTable *nftables.Table
	for _, t := range tables {
		if t.Name == "filter" && t.Family == nftables.TableFamilyINet {
			filterTable = t
			break
		}
	}
	if filterTable == nil {
		return nil, fmt.Errorf("table inet filter not found — apply deploy/nftables-filter.conf first")
	}

	sets, err := conn.GetSets(filterTable)
	if err != nil {
		return nil, fmt.Errorf("GetSets: %w", err)
	}

	var blocklist *nftables.Set
	for _, s := range sets {
		if s.Name == "blocklist" {
			blocklist = s
			break
		}
	}
	if blocklist == nil {
		return nil, fmt.Errorf("set blocklist not found — apply deploy/nftables-filter.conf first")
	}

	return &BlocklistManager{conn: conn, tbl: filterTable, blocklist: blocklist}, nil
}

// Block adds an IP to the blocklist set.
func (b *BlocklistManager) Block(ip string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		return fmt.Errorf("invalid IP: %q", ip)
	}

	err := b.conn.SetAddElements(b.blocklist, []nftables.SetElement{
		{Key: parsed},
	})
	if err != nil {
		return fmt.Errorf("SetAddElements blocklist: %w", err)
	}
	return b.conn.Flush()
}

