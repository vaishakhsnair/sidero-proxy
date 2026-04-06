package registration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/config"
)

type ProxyInfo struct {
	Subnet      string `json:"subnet"`
	TailscaleIP string `json:"tailscale_ip"`
	RegisteredAt int64  `json:"registered_at"`
}

// Register ensures this proxy has a subnet allocated in Redis.
// Returns proxyNum (e.g. 1 for 10.1.0.0/16) and the subnet string.
func Register(ctx context.Context, rdb *redis.Client, proxyID string) (proxyNum int, subnet string, err error) {
	// Check if already registered
	val, err := rdb.HGet(ctx, "proxy:registry", proxyID).Result()
	if err == nil {
		var info ProxyInfo
		if jsonErr := json.Unmarshal([]byte(val), &info); jsonErr != nil {
			return 0, "", fmt.Errorf("unmarshal proxy info: %w", jsonErr)
		}
		// Parse proxy number from subnet (10.{n}.0.0/16)
		var a, b int
		fmt.Sscanf(info.Subnet, "10.%d.%d.0/16", &a, &b)
		return a, info.Subnet, nil
	}
	if err != redis.Nil {
		return 0, "", fmt.Errorf("HGET proxy:registry: %w", err)
	}

	// Allocate new subnet slot
	n, err := rdb.Incr(ctx, "proxy:subnet_counter").Result()
	if err != nil {
		return 0, "", fmt.Errorf("INCR proxy:subnet_counter: %w", err)
	}
	if n > 254 {
		return 0, "", fmt.Errorf("proxy subnet space exhausted (max 254 proxies)")
	}

	sub := fmt.Sprintf("10.%d.0.0/16", n)
	info := ProxyInfo{
		Subnet:       sub,
		RegisteredAt: time.Now().Unix(),
	}
	data, _ := json.Marshal(info)

	if err := rdb.HSet(ctx, "proxy:registry", proxyID, data).Err(); err != nil {
		return 0, "", fmt.Errorf("HSET proxy:registry: %w", err)
	}

	return int(n), sub, nil
}

// WriteNodeMap writes node public_ip → tailscale_ip/name mappings to Redis.
// Called on every proxy startup; all proxies write the same data.
func WriteNodeMap(ctx context.Context, rdb *redis.Client, nodes []config.NodeConfig) error {
	for _, node := range nodes {
		data, _ := json.Marshal(map[string]string{
			"tailscale_ip": node.TailscaleIP,
			"name":         node.Name,
		})
		if err := rdb.Set(ctx, "node:"+node.PublicIP, data, 0).Err(); err != nil {
			return fmt.Errorf("SET node:%s: %w", node.PublicIP, err)
		}
	}
	return nil
}
