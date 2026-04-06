package ban

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/nat"
)

// BanCache is a thread-safe in-memory set of banned real IPs.
type BanCache struct {
	mu     sync.RWMutex
	banned map[string]struct{}
}

func newBanCache() *BanCache {
	return &BanCache{banned: make(map[string]struct{})}
}

func (b *BanCache) IsBanned(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.banned[ip]
	return ok
}

func (b *BanCache) Ban(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.banned[ip] = struct{}{}
}

// BanEvent is the message published to ban_events.
type BanEvent struct {
	RealIP      string            `json:"real_ip"`
	InternalIPs map[string]string `json:"internal_ips"` // proxy → internalIP
	Reason      string            `json:"reason"`
	TS          int64             `json:"ts"`
}

// Enforcer loads ban state from Redis and subscribes to ban_events.
type Enforcer struct {
	cache     *BanCache
	blocklist *nat.BlocklistManager
	rdb       *redis.Client
	proxyID   string
}

func NewEnforcer(rdb *redis.Client, proxyID string, blocklist *nat.BlocklistManager) *Enforcer {
	return &Enforcer{
		cache:     newBanCache(),
		blocklist: blocklist,
		rdb:       rdb,
		proxyID:   proxyID,
	}
}

// IsBanned returns true if the real IP is in the local ban cache.
func (e *Enforcer) IsBanned(ip string) bool {
	return e.cache.IsBanned(ip)
}

// LoadFromRedis populates the ban cache and nftables blocklist from Redis at startup.
func (e *Enforcer) LoadFromRedis(ctx context.Context) error {
	// Load real IPs
	realIPs, err := e.rdb.SMembers(ctx, "banned:real").Result()
	if err != nil {
		return fmt.Errorf("SMEMBERS banned:real: %w", err)
	}
	for _, ip := range realIPs {
		e.cache.Ban(ip)
	}

	// Load internal IPs for this proxy into nftables blocklist
	internalIPs, err := e.rdb.SMembers(ctx, "banned:internal:"+e.proxyID).Result()
	if err != nil {
		return fmt.Errorf("SMEMBERS banned:internal:%s: %w", e.proxyID, err)
	}
	for _, ip := range internalIPs {
		if err := e.blocklist.Block(ip); err != nil {
			log.Printf("warn: block internal IP %s: %v", ip, err)
		}
	}

	log.Printf("enforcer: loaded %d banned real IPs, %d internal IPs", len(realIPs), len(internalIPs))
	return nil
}

// Subscribe blocks, listening on ban_events and updating the cache + blocklist.
func (e *Enforcer) Subscribe(ctx context.Context) error {
	sub := e.rdb.Subscribe(ctx, "ban_events")
	defer sub.Close()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			var event BanEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				log.Printf("ban_events: bad payload: %v", err)
				continue
			}
			e.cache.Ban(event.RealIP)

			// Block the internal IP assigned to this proxy
			if internalIP, ok := event.InternalIPs[e.proxyID]; ok {
				if err := e.blocklist.Block(internalIP); err != nil {
					log.Printf("warn: block internal IP %s: %v", internalIP, err)
				}
			}
		}
	}
}
