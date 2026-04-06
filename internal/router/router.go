package router

import (
	"context"
	"fmt"
	"log"
	"net"

	"sidero-proxy/internal/assignment"
	"sidero-proxy/internal/ban"
	"sidero-proxy/internal/config"
	"sidero-proxy/internal/forwarder"
	"sidero-proxy/internal/nat"
	"sidero-proxy/internal/tproxy"
)

// Router accepts TPROXY-intercepted connections and forwards them to backend nodes.
type Router struct {
	cfg      *config.Config
	nodeMap  map[string]string // public_ip → tailscale_ip (read-only after init)
	assigner *assignment.Assigner
	enforcer *ban.Enforcer
	natMgr   *nat.NATManager
}

func New(
	cfg *config.Config,
	nodeMap map[string]string,
	assigner *assignment.Assigner,
	enforcer *ban.Enforcer,
	natMgr *nat.NATManager,
) *Router {
	return &Router{
		cfg:      cfg,
		nodeMap:  nodeMap,
		assigner: assigner,
		enforcer: enforcer,
		natMgr:   natMgr,
	}
}

// Start begins accepting connections. Blocks until ctx is cancelled.
func (r *Router) Start(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", r.cfg.TProxyPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	log.Printf("router: listening on %s", addr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("accept error: %v", err)
			continue
		}
		go r.handleConn(ctx, conn)
	}
}

func (r *Router) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Step 1: recover original destination
	dstIP, dstPort, err := tproxy.GetOriginalDst(conn)
	if err != nil {
		log.Printf("GetOriginalDst: %v", err)
		return
	}

	// Step 2: look up backend tailscale IP
	tailscaleIP, ok := r.nodeMap[dstIP]
	if !ok {
		log.Printf("unknown dst %s — no node for that public IP", dstIP)
		return
	}

	// Step 3: get source (real player) IP
	srcIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		log.Printf("SplitHostPort: %v", err)
		return
	}

	// Step 4: check ban cache
	if r.enforcer.IsBanned(srcIP) {
		log.Printf("banned IP %s — dropping", srcIP)
		return
	}

	// Step 5: assign internal IP
	internalIP, err := r.assigner.Assign(ctx, srcIP)
	if err != nil {
		log.Printf("assign %s: %v", srcIP, err)
		return
	}
	if internalIP == "BANNED" {
		log.Printf("IP %s is banned (Redis) — dropping", srcIP)
		return
	}

	// Step 6: add NAT map entry (SNAT: srcIP → internalIP)
	if err := r.natMgr.Add(srcIP, internalIP); err != nil {
		log.Printf("nat add %s→%s: %v", srcIP, internalIP, err)
		return
	}
	defer func() {
		if err := r.natMgr.Delete(srcIP); err != nil {
			log.Printf("nat delete %s: %v", srcIP, err)
		}
	}()

	// Step 7: dial the backend
	backendAddr := net.JoinHostPort(tailscaleIP, fmt.Sprintf("%d", dstPort))
	backend, err := net.Dial("tcp", backendAddr)
	if err != nil {
		log.Printf("dial %s: %v", backendAddr, err)
		return
	}
	defer backend.Close()

	// Step 8: forward bidirectionally
	forwarder.Forward(conn, backend)
}
