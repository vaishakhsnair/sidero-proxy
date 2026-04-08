package router

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
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

func (f *fakeNAT) Add(_ context.Context, realIP, internalIP string) error {
	f.added = append(f.added, realIP+"->"+internalIP)
	return nil
}

func (f *fakeNAT) Delete(_ context.Context, realIP string) error {
	f.deleted = append(f.deleted, realIP)
	return nil
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
	r := New(cfg, fakeAssigner{internalIP: "10.1.0.5"}, nat, slog.Default())
	r.listen = func(_, _ string) (net.Listener, error) { return routerLn, nil }
	r.originalDst = func(_ net.Conn) (string, int, error) {
		return "203.0.113.10", 25565, nil
	}
	r.dial = func(_, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		gotPort, err := strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
		wantPort := 25565
		if gotPort != wantPort {
			t.Fatalf("dialed port = %d, want %d", gotPort, wantPort)
		}
		return net.Dial("tcp", backendLn.Addr().String())
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
}
