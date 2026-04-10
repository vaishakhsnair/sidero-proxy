package router

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"sidero-proxy/internal/config"
	"sidero-proxy/internal/minecraft"
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
		HealthPort:    18080,
		PortRange:     config.PortRange{Start: 25565, End: 25565},
		Servers: []config.Server{{
			Name:          "node-a",
			ProxyPublicIP: "203.0.113.10",
			Hostnames:     []string{"ingress-1.sidero.net"},
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

	payload := append(buildHandshakePacket("ingress-1.sidero.net", 25565), []byte("ping")...)
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}

	go func() {
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(clientToBackend, buf); err != nil {
			return
		}
		if !bytes.Equal(buf[:len(payload)-4], payload[:len(payload)-4]) {
			t.Errorf("backend handshake mismatch")
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

func TestRouterFallsBackToOriginalIPForLegacyRawIPConnects(t *testing.T) {
	t.Parallel()

	cfg := &config.ProxyConfig{
		ProxyID:       "proxy-1",
		RedisAddr:     "unused",
		IPTTLSeconds:  60,
		InterceptPort: 19000,
		HealthPort:    18080,
		PortRange:     config.PortRange{Start: 25565, End: 25565},
		Servers: []config.Server{{
			Name:          "node-a",
			ProxyPublicIP: "203.0.113.10",
			Hostnames:     []string{"ingress-1.sidero.net"},
			BackendIP:     "127.0.0.1",
		}},
	}

	nat := &fakeNAT{}
	clientToBackend, backendToRouter := net.Pipe()
	defer clientToBackend.Close()
	defer backendToRouter.Close()

	dialer := &fakeDialer{conn: backendToRouter}
	r := New(cfg, fakeAssigner{internalIP: "10.1.0.5"}, nat, dialer, slog.Default())
	server, routeKey, routeType, ok := r.resolveServer(mustHandshake(t, "203.0.113.10", 25565), nil, "203.0.113.10", 25565)
	if !ok {
		t.Fatal("resolveServer() = not ok, want fallback route")
	}
	if routeType != "original_ip_fallback" || routeKey != "203.0.113.10" || server.Name != "node-a" {
		t.Fatalf("route = (%s, %s, %+v), want original_ip_fallback to node-a", routeType, routeKey, server)
	}
}

func TestRouterRejectsUnknownHostname(t *testing.T) {
	t.Parallel()

	cfg := &config.ProxyConfig{
		ProxyID:       "proxy-1",
		RedisAddr:     "unused",
		IPTTLSeconds:  60,
		InterceptPort: 19000,
		HealthPort:    18080,
		PortRange:     config.PortRange{Start: 25565, End: 25565},
		Servers: []config.Server{{
			Name:          "node-a",
			ProxyPublicIP: "203.0.113.10",
			Hostnames:     []string{"ingress-1.sidero.net"},
			BackendIP:     "127.0.0.1",
		}},
	}

	r := New(cfg, fakeAssigner{internalIP: "10.1.0.5"}, &fakeNAT{}, &fakeDialer{}, slog.Default())
	_, routeKey, routeType, ok := r.resolveServer(mustHandshake(t, "guessed-ingress.sidero.net", 25565), nil, "203.0.113.10", 25565)
	if ok {
		t.Fatal("resolveServer() = ok, want rejection")
	}
	if routeType != "unknown_hostname" || routeKey != "guessed-ingress.sidero.net" {
		t.Fatalf("route = (%s, %s), want unknown hostname", routeType, routeKey)
	}
}

func mustHandshake(t *testing.T, host string, port int) minecraft.Handshake {
	t.Helper()
	raw, handshake, err := minecraft.ReadHandshakePacket(bytes.NewReader(buildHandshakePacket(host, port)))
	if err != nil || len(raw) == 0 {
		t.Fatalf("ReadHandshakePacket() error = %v", err)
	}
	return handshake
}

func buildHandshakePacket(host string, port int) []byte {
	body := make([]byte, 0, 64)
	body = append(body, encodeVarInt(0)...)
	body = append(body, encodeVarInt(769)...)
	body = append(body, encodeString(host)...)
	body = append(body, byte(port>>8), byte(port))
	body = append(body, encodeVarInt(2)...)
	return append(encodeVarInt(len(body)), body...)
}

func encodeString(value string) []byte {
	raw := []byte(value)
	return append(encodeVarInt(len(raw)), raw...)
}

func encodeVarInt(value int) []byte {
	if value == 0 {
		return []byte{0}
	}
	buf := make([]byte, 0, 5)
	for value != 0 {
		temp := byte(value & 0x7F)
		value >>= 7
		if value != 0 {
			temp |= 0x80
		}
		buf = append(buf, temp)
	}
	return buf
}
