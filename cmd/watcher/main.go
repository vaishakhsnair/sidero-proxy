package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/ban"
)

type watcherConfig struct {
	RedisAddr     string `json:"redis_addr"`
	RedisPassword string `json:"redis_password"`
	VolumesRoot   string `json:"volumes_root"`
}

func loadWatcherConfig(path string) (*watcherConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	var cfg watcherConfig
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if cfg.VolumesRoot == "" {
		return nil, fmt.Errorf("volumes_root is required")
	}
	return &cfg, nil
}

func main() {
	cfgPath := flag.String("config", "watcher.json", "path to watcher config file")
	flag.Parse()

	cfg, err := loadWatcherConfig(*cfgPath)
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

	w := ban.NewWatcher(cfg.VolumesRoot, rdb)
	log.Printf("mcwatcher: watching %s", cfg.VolumesRoot)
	if err := w.Start(ctx); err != nil {
		log.Fatalf("watcher: %v", err)
	}
}
