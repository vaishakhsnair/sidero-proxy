package failover

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"sidero-proxy/internal/config"
)

type HealthChecker interface {
	Healthy(ctx context.Context, url string) error
}

type DNSRecord struct {
	ID      string
	Name    string
	Content string
	TTL     int
	Type    string
}

type DNSClient interface {
	ListARecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error)
	DeleteRecord(ctx context.Context, zoneID, recordID string) error
	CreateARecord(ctx context.Context, zoneID, name, content string, ttl int) error
}

type RegionState struct {
	HealthySince time.Time
	OnFallback   bool
}

type Controller struct {
	cfg      *config.FailoverConfig
	checker  HealthChecker
	dns      DNSClient
	logger   *slog.Logger
	now      func() time.Time
	regions  map[string]config.FailoverRegion
	state    map[string]RegionState
	fallback config.FailoverRegion
}

func NewController(cfg *config.FailoverConfig, checker HealthChecker, dns DNSClient, logger *slog.Logger) *Controller {
	regions := make(map[string]config.FailoverRegion, len(cfg.Regions))
	for _, region := range cfg.Regions {
		regions[region.Name] = region
	}
	return &Controller{
		cfg:      cfg,
		checker:  checker,
		dns:      dns,
		logger:   logger,
		now:      time.Now,
		regions:  regions,
		state:    make(map[string]RegionState),
		fallback: regions[cfg.FallbackRegion],
	}
}

func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Duration(c.cfg.ProbeIntervalSeconds) * time.Second)
	defer ticker.Stop()

	if err := c.Reconcile(ctx); err != nil {
		c.logger.Error("failover reconcile", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.Reconcile(ctx); err != nil {
				c.logger.Error("failover reconcile", "error", err)
			}
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	fallbackHealthy := c.checker.Healthy(ctx, c.fallback.HealthURL) == nil
	regionHealth := make(map[string]error, len(c.cfg.Regions))

	for _, hostname := range c.cfg.Hostnames {
		primary := c.regions[hostname.Region]
		desiredRegion := primary

		err, ok := regionHealth[hostname.Region]
		if !ok {
			err = c.checker.Healthy(ctx, primary.HealthURL)
			regionHealth[hostname.Region] = err
		}
		now := c.now()
		state := c.state[hostname.Region]

		if err == nil {
			if state.HealthySince.IsZero() {
				state.HealthySince = now
			}
			if state.OnFallback && c.cfg.FailbackWindowSeconds > 0 && now.Sub(state.HealthySince) < time.Duration(c.cfg.FailbackWindowSeconds)*time.Second {
				desiredRegion = c.fallback
			} else {
				desiredRegion = primary
				state.OnFallback = false
			}
		} else {
			state.HealthySince = time.Time{}
			if fallbackHealthy {
				desiredRegion = c.fallback
				state.OnFallback = true
			}
		}

		c.state[hostname.Region] = state
		if err := c.ensureHostname(ctx, hostname, desiredRegion); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) ensureHostname(ctx context.Context, hostname config.FailoverHostname, region config.FailoverRegion) error {
	records, err := c.dns.ListARecords(ctx, c.cfg.Cloudflare.ZoneID, hostname.Name)
	if err != nil {
		return fmt.Errorf("list records for %s: %w", hostname.Name, err)
	}

	currentIPs := make([]string, 0, len(records))
	for _, record := range records {
		currentIPs = append(currentIPs, record.Content)
	}
	slices.Sort(currentIPs)

	desiredIPs := append([]string(nil), region.AnswerIPs...)
	slices.Sort(desiredIPs)

	if slices.Equal(currentIPs, desiredIPs) {
		return nil
	}

	for _, record := range records {
		if err := c.dns.DeleteRecord(ctx, c.cfg.Cloudflare.ZoneID, record.ID); err != nil {
			return fmt.Errorf("delete record %s for %s: %w", record.ID, hostname.Name, err)
		}
	}
	for _, ip := range region.AnswerIPs {
		if err := c.dns.CreateARecord(ctx, c.cfg.Cloudflare.ZoneID, hostname.Name, ip, hostname.TTL); err != nil {
			return fmt.Errorf("create record for %s -> %s: %w", hostname.Name, ip, err)
		}
	}
	c.logger.Info("hostname reconciled", "hostname", hostname.Name, "region", region.Name, "answer_ips", region.AnswerIPs)
	return nil
}
