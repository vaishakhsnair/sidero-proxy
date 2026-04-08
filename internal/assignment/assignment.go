package assignment

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"sidero-proxy/internal/registration"
)

//go:embed assign.lua
var assignLua string

var assignScript = redis.NewScript(assignLua)

type ReverseMapping struct {
	RealIP string `json:"real_ip"`
	Proxy  string `json:"proxy"`
}

type Service struct {
	rdb      *redis.Client
	proxyID  string
	proxyNum int
	ttl      time.Duration
}

func New(rdb *redis.Client, proxyID string, proxyNum int, ttlSeconds int) *Service {
	return &Service{
		rdb:      rdb,
		proxyID:  proxyID,
		proxyNum: proxyNum,
		ttl:      time.Duration(ttlSeconds) * time.Second,
	}
}

func (s *Service) Assign(ctx context.Context, realIP string) (string, error) {
	return s.runAssign(ctx, realIP, s.proxyID, s.proxyNum, false)
}

func (s *Service) ReserveForAllProxies(ctx context.Context, realIP string, proxies map[string]registration.ProxyInfo) (map[string]string, error) {
	ids := registration.SortedProxyIDs(proxies)
	out := make(map[string]string, len(ids))
	for _, proxyID := range ids {
		proxyNum, err := registration.ProxyNumFromSubnet(proxies[proxyID].Subnet)
		if err != nil {
			return nil, err
		}
		internalIP, err := s.runAssign(ctx, realIP, proxyID, proxyNum, true)
		if err != nil {
			return nil, fmt.Errorf("reserve mapping for %s: %w", proxyID, err)
		}
		out[proxyID] = internalIP
	}
	return out, nil
}

func (s *Service) LookupReverse(ctx context.Context, internalIP string) (ReverseMapping, error) {
	val, err := s.rdb.Get(ctx, "rmap:"+internalIP).Result()
	if err != nil {
		return ReverseMapping{}, fmt.Errorf("lookup reverse mapping for %s: %w", internalIP, err)
	}
	var mapping ReverseMapping
	if err := json.Unmarshal([]byte(val), &mapping); err != nil {
		return ReverseMapping{}, fmt.Errorf("decode reverse mapping for %s: %w", internalIP, err)
	}
	return mapping, nil
}

func (s *Service) LookupMappings(ctx context.Context, realIP string) (map[string]string, error) {
	vals, err := s.rdb.HGetAll(ctx, "map:"+realIP).Result()
	if err != nil {
		return nil, fmt.Errorf("lookup mappings for %s: %w", realIP, err)
	}
	return vals, nil
}

func (s *Service) runAssign(ctx context.Context, realIP, proxyID string, proxyNum int, reserve bool) (string, error) {
	reserveFlag := "0"
	if reserve {
		reserveFlag = "1"
	}
	reply, err := assignScript.Run(
		ctx,
		s.rdb,
		[]string{realIP},
		proxyID,
		strconv.Itoa(proxyNum),
		strconv.Itoa(int(s.ttl.Seconds())),
		reserveFlag,
	).Text()
	if err != nil {
		return "", fmt.Errorf("run assignment script: %w", err)
	}
	return reply, nil
}

func CounterToInternalIP(proxyNum int, counter int64) (string, error) {
	if proxyNum <= 0 || proxyNum >= 255 {
		return "", fmt.Errorf("invalid proxy number %d", proxyNum)
	}
	if counter <= 0 {
		return "", fmt.Errorf("counter must be positive")
	}
	octet3 := (counter / 256) % 256
	octet4 := counter % 256
	return fmt.Sprintf("10.%d.%d.%d", proxyNum, octet3, octet4), nil
}

func SortedMappings(m map[string]string) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, m[k]})
	}
	return out
}
