package registration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

type ProxyInfo struct {
	Subnet       string `json:"subnet"`
	TailscaleIP  string `json:"tailscale_ip,omitempty"`
	RegisteredAt int64  `json:"registered_at"`
}

const (
	registryKey      = "proxy:registry"
	subnetCounterKey = "proxy:subnet_counter"
)

func Register(ctx context.Context, rdb *redis.Client, proxyID string) (ProxyInfo, int, error) {
	val, err := rdb.HGet(ctx, registryKey, proxyID).Result()
	if err == nil {
		var info ProxyInfo
		if err := json.Unmarshal([]byte(val), &info); err != nil {
			return ProxyInfo{}, 0, fmt.Errorf("decode proxy registry for %s: %w", proxyID, err)
		}
		proxyNum, err := ProxyNumFromSubnet(info.Subnet)
		if err != nil {
			return ProxyInfo{}, 0, err
		}
		return info, proxyNum, nil
	}
	if err != redis.Nil {
		return ProxyInfo{}, 0, fmt.Errorf("load proxy registry: %w", err)
	}

	slot, err := rdb.Incr(ctx, subnetCounterKey).Result()
	if err != nil {
		return ProxyInfo{}, 0, fmt.Errorf("allocate subnet slot: %w", err)
	}
	if slot <= 0 || slot > 254 {
		return ProxyInfo{}, 0, fmt.Errorf("proxy subnet space exhausted: %d", slot)
	}

	info := ProxyInfo{
		Subnet:       fmt.Sprintf("10.%d.0.0/16", slot),
		RegisteredAt: time.Now().Unix(),
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		return ProxyInfo{}, 0, fmt.Errorf("encode proxy registry: %w", err)
	}
	if err := rdb.HSet(ctx, registryKey, proxyID, encoded).Err(); err != nil {
		return ProxyInfo{}, 0, fmt.Errorf("write proxy registry: %w", err)
	}
	return info, int(slot), nil
}

func ListProxies(ctx context.Context, rdb *redis.Client) (map[string]ProxyInfo, error) {
	raw, err := rdb.HGetAll(ctx, registryKey).Result()
	if err != nil {
		return nil, fmt.Errorf("read proxy registry: %w", err)
	}
	out := make(map[string]ProxyInfo, len(raw))
	for proxyID, val := range raw {
		var info ProxyInfo
		if err := json.Unmarshal([]byte(val), &info); err != nil {
			return nil, fmt.Errorf("decode proxy registry for %s: %w", proxyID, err)
		}
		out[proxyID] = info
	}
	return out, nil
}

func SortedProxyIDs(proxies map[string]ProxyInfo) []string {
	ids := make([]string, 0, len(proxies))
	for id := range proxies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func ProxyNumFromSubnet(subnet string) (int, error) {
	ip, network, err := net.ParseCIDR(subnet)
	if err != nil {
		return 0, fmt.Errorf("parse subnet %q: %w", subnet, err)
	}
	if ip4 := ip.To4(); ip4 == nil {
		return 0, fmt.Errorf("subnet %q is not IPv4", subnet)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 16 {
		return 0, fmt.Errorf("subnet %q must be /16", subnet)
	}
	ip4 := ip.To4()
	if ip4[0] != 10 || ip4[2] != 0 || ip4[3] != 0 {
		return 0, fmt.Errorf("subnet %q must match 10.<n>.0.0/16", subnet)
	}
	if ip4[1] == 0 || ip4[1] == 255 {
		return 0, fmt.Errorf("subnet %q has invalid proxy slot", subnet)
	}
	return int(ip4[1]), nil
}
