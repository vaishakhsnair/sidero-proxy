package router

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"sidero-proxy/internal/config"
	"sidero-proxy/internal/forwarder"
	"sidero-proxy/internal/origdst"
)

type NATManager interface {
	Add(ctx context.Context, realIP, internalIP string) error
	Delete(ctx context.Context, realIP string) error
}

type Assigner interface {
	Assign(ctx context.Context, realIP string) (string, error)
}

type BackendDialer interface {
	Dial(ctx context.Context, sourceIP, destIP string, destPort int) (net.Conn, error)
}

type ListenFunc func(network, address string) (net.Listener, error)
type OriginalDstFunc func(conn net.Conn) (string, int, error)

type Router struct {
	cfg         *config.ProxyConfig
	assigner    Assigner
	nat         NATManager
	dialer      BackendDialer
	logger      *slog.Logger
	listen      ListenFunc
	originalDst OriginalDstFunc
	backendByIP map[string]config.Server
}

func New(cfg *config.ProxyConfig, assigner Assigner, nat NATManager, dialer BackendDialer, logger *slog.Logger) *Router {
	backendByIP := make(map[string]config.Server, len(cfg.Servers))
	for _, server := range cfg.Servers {
		backendByIP[server.ProxyPublicIP] = server
	}
	return &Router{
		cfg:         cfg,
		assigner:    assigner,
		nat:         nat,
		dialer:      dialer,
		logger:      logger,
		listen:      net.Listen,
		originalDst: origdst.Get,
		backendByIP: backendByIP,
	}
}

func (r *Router) Start(ctx context.Context) error {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(r.cfg.InterceptPort))
	ln, err := r.listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	r.logger.Info("intercept listener started", "listen_addr", addr, "range_start", r.cfg.PortRange.Start, "range_end", r.cfg.PortRange.End, "public_ip_mappings", len(r.cfg.Servers))

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.serveListener(ctx, ln); err != nil {
			errCh <- err
		}
	}()
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (r *Router) serveListener(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				r.logger.Warn("temporary accept failure", "error", err, "listen_addr", ln.Addr().String())
				continue
			}
			return fmt.Errorf("accept on %s: %w", ln.Addr().String(), err)
		}

		go r.handleConn(ctx, conn)
	}
}

func (r *Router) handleConn(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()

	realIP, _, err := net.SplitHostPort(client.RemoteAddr().String())
	if err != nil {
		r.logger.Error("split client address", "error", err, "remote_addr", client.RemoteAddr().String())
		return
	}

	internalIP, err := r.assigner.Assign(ctx, realIP)
	if err != nil {
		r.logger.Error("assign internal ip", "error", err, "real_ip", realIP)
		return
	}

	if err := r.nat.Add(ctx, realIP, internalIP); err != nil {
		r.logger.Error("install nat mapping", "error", err, "real_ip", realIP, "internal_ip", internalIP)
		return
	}
	defer func() {
		if err := r.nat.Delete(context.Background(), realIP); err != nil {
			r.logger.Warn("remove nat mapping", "error", err, "real_ip", realIP)
		}
	}()

	originalIP, originalPort, err := r.originalDst(client)
	if err != nil {
		r.logger.Error("resolve original destination", "error", err)
		return
	}

	server, ok := r.backendByIP[originalIP]
	if !ok {
		r.logger.Error("no backend mapping for original destination", "original_ip", originalIP, "original_port", originalPort)
		return
	}

	backendAddr := net.JoinHostPort(server.BackendIP, strconv.Itoa(originalPort))
	backend, err := r.dialer.Dial(ctx, internalIP, server.BackendIP, originalPort)
	if err != nil {
		r.logger.Error("dial backend", "error", err, "backend", backendAddr, "server_name", server.Name, "internal_ip", internalIP)
		return
	}
	defer func() { _ = backend.Close() }()

	r.logger.Info("connection established", "server_name", server.Name, "real_ip", realIP, "internal_ip", internalIP, "original_ip", originalIP, "original_port", originalPort, "backend", backendAddr)
	forwarder.Proxy(client, backend)
}
