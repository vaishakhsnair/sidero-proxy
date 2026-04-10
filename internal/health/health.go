package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

type ListenerState struct {
	ready atomic.Bool
}

func (s *ListenerState) SetReady(ready bool) {
	s.ready.Store(ready)
}

func (s *ListenerState) Ready() bool {
	return s.ready.Load()
}

type Runner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %v failed: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type RedisPinger interface {
	Ping(ctx context.Context) error
}

type RedisPingerFunc func(ctx context.Context) error

func (f RedisPingerFunc) Ping(ctx context.Context) error {
	return f(ctx)
}

type Checker struct {
	Listener *ListenerState
	Redis    RedisPinger
	Runner   Runner
	Subnet   string
}

func (c *Checker) Check(ctx context.Context) error {
	if c.Listener == nil || !c.Listener.Ready() {
		return fmt.Errorf("intercept listener not ready")
	}
	if c.Redis == nil {
		return fmt.Errorf("redis pinger is not configured")
	}
	if err := c.Redis.Ping(ctx); err != nil {
		return fmt.Errorf("redis unhealthy: %w", err)
	}
	if c.Runner == nil {
		return fmt.Errorf("health runner is not configured")
	}
	if err := c.checkNFT(ctx); err != nil {
		return err
	}
	if err := c.checkRoute(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Checker) checkNFT(ctx context.Context) error {
	out, err := c.Runner.Output(ctx, "nft", "list", "tables")
	if err != nil {
		return fmt.Errorf("list nftables tables: %w", err)
	}
	tables := string(out)
	required := []string{"table ip mcproxy_filter", "table ip mcproxy_intercept", "table ip mcproxy_nat"}
	for _, table := range required {
		if !strings.Contains(tables, table) {
			return fmt.Errorf("required nftables table missing: %s", table)
		}
	}
	return nil
}

func (c *Checker) checkRoute(ctx context.Context) error {
	out, err := c.Runner.Output(ctx, "ip", "route", "show", "table", "local")
	if err != nil {
		return fmt.Errorf("list local routes: %w", err)
	}
	expected := "local " + c.Subnet + " dev lo"
	if !strings.Contains(string(out), expected) {
		return fmt.Errorf("required local route missing: %s", expected)
	}
	return nil
}

type Server struct {
	checker *Checker
}

func NewServer(checker *Checker) *Server {
	return &Server{checker: checker}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := map[string]string{"status": "ok"}
	code := http.StatusOK
	if err := s.checker.Check(ctx); err != nil {
		code = http.StatusServiceUnavailable
		resp["status"] = "error"
		resp["error"] = err.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
