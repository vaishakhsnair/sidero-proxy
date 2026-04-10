package minecraft

import (
	"bytes"
	"testing"
)

func TestReadHandshakePacket(t *testing.T) {
	t.Parallel()

	packet := buildHandshakePacket("Ingress-123.Sidero.net\x00extra", 25565)

	raw, handshake, err := ReadHandshakePacket(bytes.NewReader(packet))
	if err != nil {
		t.Fatalf("ReadHandshakePacket() error = %v", err)
	}
	if !bytes.Equal(raw, packet) {
		t.Fatalf("raw packet mismatch")
	}
	if handshake.Hostname != "ingress-123.sidero.net" {
		t.Fatalf("Hostname = %q, want ingress-123.sidero.net", handshake.Hostname)
	}
	if handshake.Port != 25565 {
		t.Fatalf("Port = %d, want 25565", handshake.Port)
	}
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
