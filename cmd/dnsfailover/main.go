package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"sidero-proxy/internal/config"
	"sidero-proxy/internal/failover"
	"sidero-proxy/internal/logx"
)

func main() {
	configPath := flag.String("config", "failover.json", "path to DNS failover config")
	flag.Parse()

	logger := logx.New("dnsfailover")
	cfg, err := config.LoadFailover(*configPath)
	if err != nil {
		panic(fmt.Errorf("load failover config: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	controller := failover.NewController(
		cfg,
		failover.NewHTTPHealthChecker(3*time.Second),
		failover.NewCloudflareClient(cfg.Cloudflare.APIToken),
		logger,
	)
	if err := controller.Run(ctx); err != nil {
		panic(fmt.Errorf("run dns failover controller: %w", err))
	}
}
