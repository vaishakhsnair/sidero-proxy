package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"

	"sidero-proxy/internal/assignment"
	"sidero-proxy/internal/config"
	"sidero-proxy/internal/filter"
	"sidero-proxy/internal/health"
	"sidero-proxy/internal/logx"
	"sidero-proxy/internal/nat"
	"sidero-proxy/internal/redisutil"
	"sidero-proxy/internal/registration"
	"sidero-proxy/internal/router"
	"sidero-proxy/internal/source"
)

func main() {
	configPath := flag.String("config", "config.json", "path to proxy config")
	flag.Parse()

	logger := logx.New("proxy")

	cfg, err := config.LoadProxy(*configPath)
	if err != nil {
		panic(fmt.Errorf("load proxy config: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rdb := redisutil.NewClient(cfg.RedisAddr, cfg.RedisPassword)
	if err := redisutil.Ping(ctx, rdb); err != nil {
		panic(err)
	}
	defer func() { _ = rdb.Close() }()

	info, proxyNum, err := registration.Register(ctx, rdb, cfg.ProxyID)
	if err != nil {
		panic(fmt.Errorf("register proxy: %w", err))
	}
	logger.Info("proxy registered", "proxy_id", cfg.ProxyID, "subnet", info.Subnet, "proxy_num", proxyNum)
	logger.Info("proxy port range configured", "start_port", cfg.PortRange.Start, "end_port", cfg.PortRange.End, "intercept_port", cfg.InterceptPort, "public_ip_mappings", len(cfg.Servers))

	routeManager := source.NewRouteManager(source.ExecRunner{})
	if err := routeManager.EnsureLocalSubnet(ctx, info.Subnet); err != nil {
		panic(fmt.Errorf("ensure local route for %s: %w", info.Subnet, err))
	}

	natManager := nat.NewNativeNATManager(nat.ExecRunner{})
	if err := natManager.Ensure(ctx); err != nil {
		panic(fmt.Errorf("ensure nftables nat rules: %w", err))
	}
	redirectManager := nat.NewProxyRedirectManager(nat.ExecRunner{})
	publicIPs := make([]string, 0, len(cfg.Servers))
	for _, server := range cfg.Servers {
		publicIPs = append(publicIPs, server.ProxyPublicIP)
	}
	filterManager := filter.NewManager(filter.ExecRunner{})
	if err := filterManager.Ensure(ctx, publicIPs, cfg.PortRange.Start, cfg.PortRange.End); err != nil {
		panic(fmt.Errorf("ensure proxy prefilter rules: %w", err))
	}
	if err := redirectManager.Ensure(ctx, publicIPs, cfg.PortRange.Start, cfg.PortRange.End, cfg.InterceptPort); err != nil {
		panic(fmt.Errorf("ensure proxy redirect rules: %w", err))
	}

	assigner := assignment.New(rdb, cfg.ProxyID, proxyNum, cfg.IPTTLSeconds)
	listenerState := &health.ListenerState{}
	checker := &health.Checker{
		Listener: listenerState,
		Redis: health.RedisPingerFunc(func(ctx context.Context) error {
			return redisutil.Ping(ctx, rdb)
		}),
		Runner: health.ExecRunner{},
		Subnet: info.Subnet,
	}
	healthServer := &http.Server{
		Addr:    net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.HealthPort)),
		Handler: health.NewServer(checker).Handler(),
	}
	go func() {
		<-ctx.Done()
		_ = healthServer.Shutdown(context.Background())
	}()
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(fmt.Errorf("run health server: %w", err))
		}
	}()

	r := router.New(cfg, assigner, natManager, source.TransparentDialer{}, logger)
	r.SetReadyHook(listenerState.SetReady)
	if err := r.Start(ctx); err != nil {
		panic(fmt.Errorf("run proxy: %w", err))
	}
}
