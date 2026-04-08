package router

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"sidero-proxy/internal/config"
)

type fakeAssigner struct {
	internalIP string
}

func (f fakeAssigner) Assign(_ context.Context, _ string) (string, error) {
	return f.internalIP, nil
}

type fakeNAT struct {
	added   []string
	deleted []string
}

type fakeDialer struct {
	conn     net.Conn
	sourceIP string
	destIP   string
	destPort int
}

func (f *fakeNAT) Add(_ context.Context, realIP, internalIP string) error {
	f.added = append(f.added, realIP+"->"+internalIP)
	return nil
}

func (f *fakeNAT) Delete(_ context.Context, realIP string) error {
	f.deleted = append(f.deleted, realIP)
	return nil
}

func (f *fakeDialer) Dial(_ context.Context, sourceIP, destIP string, destPort int) (net.Conn, error) {
	f.sourceIP = sourceIP
	f.destIP = destIP
	f.destPort = destPort
	return f.conn, nil
}

func TestRouterForwardsTrafficAndCleansUpNAT(t *testing.T) {
	t.Parallel()

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backendLn.Close()

	go func() {
		conn, err := backendLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		_, _ = conn.Write([]byte("pong"))
	}()

	routerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen router: %v", err)
	}
	defer routerLn.Close()

	cfg := &config.ProxyConfig{
		ProxyID:       "proxy-1",
		RedisAddr:     "unused",
		IPTTLSeconds:  60,
		InterceptPort: 19000,
		PortRange:     config.PortRange{Start: 25565, End: 25565},
		Servers: []config.Server{{
			Name:          "node-a",
			ProxyPublicIP: "203.0.113.10",
			BackendIP:     "127.0.0.1",
		}},
	}

	nat := &fakeNAT{}
	clientToBackend, backendToRouter := net.Pipe()
	defer clientToBackend.Close()
	defer backendToRouter.Close()

	dialer := &fakeDialer{conn: backendToRouter}
	r := New(cfg, fakeAssigner{internalIP: "10.1.0.5"}, nat, dialer, slog.Default())
	r.listen = func(_, _ string) (net.Listener, error) { return routerLn, nil }
	r.originalDst = func(_ net.Conn) (string, int, error) {
		return "203.0.113.10", 25565, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Start(ctx)
	}()

	client, err := net.Dial("tcp", routerLn.Addr().String())
	if err != nil {
		t.Fatalf("dial router: %v", err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("client write: %v", err)
	}

	go func() {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(clientToBackend, buf); err != nil {
			return
		}
		_, _ = clientToBackend.Write([]byte("pong"))
	}()

	reply := make([]byte, 4)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(reply) != "pong" {
		t.Fatalf("reply = %q, want pong", string(reply))
	}

	_ = client.Close()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not shut down")
	}

	if len(nat.added) != 1 {
		t.Fatalf("nat added = %v, want one entry", nat.added)
	}
	if len(nat.deleted) != 1 {
		t.Fatalf("nat deleted = %v, want one entry", nat.deleted)
	}
	if dialer.sourceIP != "10.1.0.5" || dialer.destIP != "127.0.0.1" || dialer.destPort != 25565 {
		t.Fatalf("dial args = %s %s %d", dialer.sourceIP, dialer.destIP, dialer.destPort)
	}
}
