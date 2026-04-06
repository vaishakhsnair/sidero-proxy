package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/assignment"
	"sidero-proxy/internal/ban"
	"sidero-proxy/internal/config"
	"sidero-proxy/internal/nat"
	"sidero-proxy/internal/registration"
	"sidero-proxy/internal/router"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	// Register proxy and get subnet allocation
	proxyNum, subnet, err := registration.Register(ctx, rdb, cfg.ProxyID)
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	log.Printf("proxy %s registered: subnet %s (num %d)", cfg.ProxyID, subnet, proxyNum)

	// Write node mappings to Redis
	if err := registration.WriteNodeMap(ctx, rdb, cfg.Nodes); err != nil {
		log.Fatalf("WriteNodeMap: %v", err)
	}

	// Build in-memory nodeMap (read-only after this point)
	nodeMap := make(map[string]string, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		nodeMap[node.PublicIP] = node.TailscaleIP
	}

	// Initialize nftables managers
	natMgr, err := nat.New()
	if err != nil {
		log.Fatalf("nat: %v", err)
	}
	// Flush stale NAT entries from a previous run
	if err := natMgr.Flush(); err != nil {
		log.Printf("warn: nat flush: %v", err)
	}

	blocklistMgr, err := nat.NewBlocklistManager()
	if err != nil {
		log.Fatalf("blocklist: %v", err)
	}

	// Initialize ban enforcer and load state from Redis
	enforcer := ban.NewEnforcer(rdb, cfg.ProxyID, blocklistMgr)
	if err := enforcer.LoadFromRedis(ctx); err != nil {
		log.Fatalf("enforcer load: %v", err)
	}

	// Initialize IP assigner and pre-warm cache
	assigner := assignment.New(rdb, cfg.ProxyID, proxyNum, cfg.IPTTLSeconds)
	if err := assigner.LoadCache(ctx); err != nil {
		log.Printf("warn: assigner load cache: %v", err)
	}

	// Subscribe to ban events in background
	go func() {
		if err := enforcer.Subscribe(ctx); err != nil {
			log.Printf("enforcer subscribe: %v", err)
		}
	}()

	// Start router
	r := router.New(cfg, nodeMap, assigner, enforcer, natMgr)
	if err := r.Start(ctx); err != nil {
		log.Fatalf("router: %v", err)
	}
}
