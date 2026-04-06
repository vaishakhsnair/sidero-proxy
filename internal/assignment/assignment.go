package assignment

import (
	"context"
	_ "embed"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

//go:embed assign.lua
var assignLua string

var assignScript = redis.NewScript(assignLua)

// Assigner handles IP assignment with an optional local cache.
type Assigner struct {
	rdb      *redis.Client
	proxyID  string
	proxyNum int
	ttl      int

	mu    sync.RWMutex
	cache map[string]string // realIP → internalIP
}

func New(rdb *redis.Client, proxyID string, proxyNum, ttl int) *Assigner {
	return &Assigner{
		rdb:      rdb,
		proxyID:  proxyID,
		proxyNum: proxyNum,
		ttl:      ttl,
		cache:    make(map[string]string),
	}
}

// Assign returns the internal IP for realIP, or "BANNED" if the IP is banned.
// Checks local cache first; falls back to Redis Lua script on miss.
func (a *Assigner) Assign(ctx context.Context, realIP string) (string, error) {
	a.mu.RLock()
	cached, ok := a.cache[realIP]
	a.mu.RUnlock()
	if ok {
		return cached, nil
	}

	result, err := assignScript.Run(ctx, a.rdb,
		[]string{realIP},
		a.proxyID,
		fmt.Sprintf("%d", a.proxyNum),
		fmt.Sprintf("%d", a.ttl),
	).Text()
	if err != nil {
		return "", fmt.Errorf("assign script: %w", err)
	}

	if result != "BANNED" {
		a.mu.Lock()
		a.cache[realIP] = result
		a.mu.Unlock()
	}
	return result, nil
}

// Evict removes a real IP from the local cache (called on ban events).
func (a *Assigner) Evict(realIP string) {
	a.mu.Lock()
	delete(a.cache, realIP)
	a.mu.Unlock()
}

// LoadCache pre-populates the local cache from Redis at startup.
func (a *Assigner) LoadCache(ctx context.Context) error {
	iter := a.rdb.Scan(ctx, 0, "map:*", 0).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		realIP := key[4:] // strip "map:"
		val, err := a.rdb.HGet(ctx, key, a.proxyID).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return fmt.Errorf("HGET %s: %w", key, err)
		}
		a.mu.Lock()
		a.cache[realIP] = val
		a.mu.Unlock()
	}
	return iter.Err()
}
