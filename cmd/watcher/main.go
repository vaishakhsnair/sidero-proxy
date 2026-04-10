package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"sidero-proxy/internal/assignment"
	"sidero-proxy/internal/config"
	"sidero-proxy/internal/logx"
	"sidero-proxy/internal/nat"
	"sidero-proxy/internal/redisutil"
	"sidero-proxy/internal/watcher"
)

func main() {
	configPath := flag.String("config", "watcher.json", "path to watcher config")
	flag.Parse()

	logger := logx.New("watcher")

	cfg, err := config.LoadWatcher(*configPath)
	if err != nil {
		panic(fmt.Errorf("load watcher config: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rdb := redisutil.NewClient(cfg.RedisAddr, cfg.RedisPassword)
	if err := redisutil.Ping(ctx, rdb); err != nil {
		panic(err)
	}
	defer func() { _ = rdb.Close() }()

	assigner := assignment.New(rdb, "", 0, 0)
	dnatManager := nat.NewNodeDNATManager(nat.ExecRunner{})
	resolver := watcher.NewDockerResolver(cfg.NodeDNAT.PublicIP, cfg.NodeDNAT.DockerNetwork, cfg.NodeDNAT.PortRange.Start, cfg.NodeDNAT.PortRange.End)
	service := watcher.New(cfg, rdb, assigner, dnatManager, resolver, logger)
	if err := service.Start(ctx); err != nil {
		panic(fmt.Errorf("run watcher: %w", err))
	}
}
