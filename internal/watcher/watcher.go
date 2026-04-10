package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/assignment"
	"sidero-proxy/internal/config"
	"sidero-proxy/internal/nat"
	"sidero-proxy/internal/registration"
)

type BannedIPEntry struct {
	IP      string `json:"ip"`
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Created string `json:"created"`
	Expires string `json:"expires"`
}

type BannedPlayerEntry struct {
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Created string `json:"created"`
	Expires string `json:"expires"`
}

type dnatManager interface {
	EnsureRange(ctx context.Context, publicIP, tailscaleInterface string, startPort, endPort int) error
	EnsureDestinations(ctx context.Context, tailscaleInterface string, destinations map[int]nat.DNATDestination) error
}

type Service struct {
	cfg        *config.WatcherConfig
	logger     *slog.Logger
	assigner   *assignment.Service
	rdb        *redis.Client
	dnat       dnatManager
	resolver   EndpointResolver
	mu         sync.Mutex
	ipSnapshot map[string]map[string]struct{}
}

func New(cfg *config.WatcherConfig, rdb *redis.Client, assigner *assignment.Service, dnat dnatManager, resolver EndpointResolver, logger *slog.Logger) *Service {
	return &Service{
		cfg:        cfg,
		logger:     logger,
		assigner:   assigner,
		rdb:        rdb,
		dnat:       dnat,
		resolver:   resolver,
		ipSnapshot: make(map[string]map[string]struct{}),
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s.dnat != nil && s.cfg.NodeDNAT.PortRange.Start > 0 {
		if err := s.ensureNodeDNAT(ctx); err != nil {
			return fmt.Errorf("ensure node dnat: %w", err)
		}
		if s.resolver != nil {
			go s.watchDocker(ctx)
		}
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer fsw.Close()

	if err := s.addWatches(fsw); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fsw.Errors:
			if err != nil {
				s.logger.Warn("filesystem watcher error", "error", err)
			}
		case event := <-fsw.Events:
			if err := s.handleEvent(ctx, fsw, event); err != nil {
				s.logger.Warn("handle watcher event", "error", err, "event", event.String())
			}
		}
	}
}

func (s *Service) watchDocker(ctx context.Context) {
	triggerCh, errCh := s.resolver.Events(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-triggerCh:
		case err, ok := <-errCh:
			if ok && err != nil {
				s.logger.Warn("docker events watcher exited", "error", err)
			}
			return
		}

		if err := s.ensureNodeDNAT(ctx); err != nil {
			s.logger.Warn("reconcile node dnat", "error", err)
		}
	}
}

func (s *Service) ensureNodeDNAT(ctx context.Context) error {
	if s.resolver != nil {
		destinations, err := s.resolver.Resolve(ctx)
		if err != nil {
			return err
		}
		if err := s.dnat.EnsureDestinations(ctx, s.cfg.NodeDNAT.TailscaleInterface, destinations); err != nil {
			return err
		}
		s.logger.Info("node dnat ensured", "tailscale_interface", s.cfg.NodeDNAT.TailscaleInterface, "container_targets", len(destinations))
		return nil
	}

	if err := s.dnat.EnsureRange(ctx, s.cfg.NodeDNAT.PublicIP, s.cfg.NodeDNAT.TailscaleInterface, s.cfg.NodeDNAT.PortRange.Start, s.cfg.NodeDNAT.PortRange.End); err != nil {
		return err
	}
	s.logger.Info("node dnat ensured", "public_ip", s.cfg.NodeDNAT.PublicIP, "tailscale_interface", s.cfg.NodeDNAT.TailscaleInterface)
	return nil
}

func (s *Service) addWatches(fsw *fsnotify.Watcher) error {
	if err := fsw.Add(s.cfg.VolumesRoot); err != nil {
		return fmt.Errorf("watch volumes root %s: %w", s.cfg.VolumesRoot, err)
	}

	entries, err := os.ReadDir(s.cfg.VolumesRoot)
	if err != nil {
		return fmt.Errorf("read volumes root %s: %w", s.cfg.VolumesRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(s.cfg.VolumesRoot, entry.Name())
		if err := fsw.Add(dir); err != nil {
			return fmt.Errorf("watch server dir %s: %w", dir, err)
		}
		if err := s.refreshSnapshots(dir); err != nil {
			s.logger.Warn("initialize watcher snapshot", "error", err, "dir", dir)
		}
	}
	return nil
}

func (s *Service) handleEvent(ctx context.Context, fsw *fsnotify.Watcher, event fsnotify.Event) error {
	if event.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			if err := fsw.Add(event.Name); err != nil {
				return fmt.Errorf("watch new directory %s: %w", event.Name, err)
			}
			return s.refreshSnapshots(event.Name)
		}
	}

	base := filepath.Base(event.Name)
	if base != "banned-ips.json" && base != "banned-players.json" {
		return nil
	}
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
		return nil
	}

	dir := filepath.Dir(event.Name)
	if base == "banned-players.json" {
		return s.handleBannedPlayers(dir)
	}
	return s.handleBannedIPs(ctx, dir)
}

func (s *Service) refreshSnapshots(dir string) error {
	entries, err := readBannedIPs(filepath.Join(dir, "banned-ips.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	snapshot := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		snapshot[entry.IP] = struct{}{}
	}
	s.mu.Lock()
	s.ipSnapshot[dir] = snapshot
	s.mu.Unlock()
	return nil
}

func (s *Service) handleBannedPlayers(dir string) error {
	players, err := readBannedPlayers(filepath.Join(dir, "banned-players.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(players) > 0 {
		s.logger.Info("observed banned players file update; username-based reconciliation is not implemented without an external username mapping", "dir", dir, "entries", len(players))
	}
	return nil
}

func (s *Service) handleBannedIPs(ctx context.Context, dir string) error {
	entries, err := readBannedIPs(filepath.Join(dir, "banned-ips.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	current := make(map[string]struct{}, len(entries))
	var newEntries []BannedIPEntry

	s.mu.Lock()
	prev := s.ipSnapshot[dir]
	if prev == nil {
		prev = make(map[string]struct{})
	}
	for _, entry := range entries {
		current[entry.IP] = struct{}{}
		if _, ok := prev[entry.IP]; !ok {
			newEntries = append(newEntries, entry)
		}
	}
	s.ipSnapshot[dir] = current
	s.mu.Unlock()

	for _, entry := range newEntries {
		if err := s.promoteIdentity(ctx, entry); err != nil {
			s.logger.Warn("promote banned identity", "error", err, "internal_ip", entry.IP, "dir", dir)
		}
	}
	return nil
}

func (s *Service) promoteIdentity(ctx context.Context, entry BannedIPEntry) error {
	mapping, err := s.assigner.LookupReverse(ctx, entry.IP)
	if err != nil {
		return err
	}

	proxies, err := registration.ListProxies(ctx, s.rdb)
	if err != nil {
		return err
	}

	reserved, err := s.assigner.ReserveForAllProxies(ctx, mapping.RealIP, proxies)
	if err != nil {
		return err
	}

	s.logger.Info("reserved identity across proxies", "real_ip", mapping.RealIP, "source_proxy", mapping.Proxy, "seed_internal_ip", entry.IP, "reserved_mappings", fmt.Sprintf("%v", reserved))
	return nil
}

func readBannedIPs(path string) ([]BannedIPEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var entries []BannedIPEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return entries, nil
}

func readBannedPlayers(path string) ([]BannedPlayerEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var entries []BannedPlayerEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return entries, nil
}
